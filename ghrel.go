// Package ci is documented in doc.go.
// cspell:ignore Ewma Kibi Rbound TCGETS vbauerster badurl copyerr defaultdest isdir
// cspell:ignore rawhex stallsig syncfail barwidth barmove noprogress barw KMGTPE
package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

// FetchOptions controls FetchGitHubRelease.
type FetchOptions struct {
	// Repo is "owner/name" of the GitHub repository.
	Repo string
	// Tag identifies the release. Empty means "latest".
	Tag string
	// AssetName is the exact asset file name to download.
	AssetName string
	// DestDir is the target directory. The final path is DestDir/AssetName.
	// When empty, the current working directory is used.
	DestDir string
	// FileMode is applied to the downloaded file. Defaults to 0o755.
	FileMode os.FileMode
	// Token is a GitHub token used for authentication. Required for private
	// repositories.
	Token string
	// SkipVerify disables sha256 verification against the asset .digest
	// field returned by the API. When false (the default) and the API
	// exposes a digest, the downloaded file is verified and a mismatch
	// causes an error.
	SkipVerify bool
	// FallbackToExisting returns the existing file at DestDir/AssetName when
	// either the API request or the download fails after all retries.
	FallbackToExisting bool
	// ConnectTimeout limits the TCP connect, TLS handshake and response. Zero
	// keeps the default of 11 seconds.
	ConnectTimeout time.Duration
	// StallTimeout aborts the transfer if less than StallLimit bytes have
	// been received during that window. Zero keeps the default of 30
	// seconds. Set to a negative value to disable the watchdog.
	StallTimeout time.Duration
	// StallLimit is the minimum acceptable throughput in bytes per
	// StallTimeout window. Zero keeps the default of 1 byte.
	StallLimit int64
	// RetryMax bounds the number of attempts. Zero keeps the default of 5.
	RetryMax int
	// RetrySleepMax caps the exponential backoff sleep. Zero keeps the
	// default of 10 seconds.
	RetrySleepMax time.Duration
	// HTTPClient overrides the default *http.Client.
	HTTPClient *http.Client
	// APIBaseURL overrides the GitHub API endpoint. Defaults to
	// https://api.github.com.
	APIBaseURL string
	// NoProgressBar disables the curl-like progress bar. By default the
	// progress bar is drawn on Stderr regardless of whether it is a TTY.
	NoProgressBar bool
	// CIMode forces the curl-like non-TTY progress renderer that prints
	// every frame on its own line, suitable for CI logs. When nil the
	// renderer is picked automatically: mpb for TTY, CI renderer
	// otherwise. Explicit true/false overrides the auto-detection.
	CIMode *bool
	// Stdout receives informational messages. Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives error and progress output. Defaults to os.Stderr.
	Stderr io.Writer
}

const (
	defaultConnectTimeout = 11 * time.Second
	defaultStallTimeout   = 30 * time.Second
	defaultStallLimit     = int64(1)
	defaultRetryMax       = 5
	defaultRetrySleepMax  = 10 * time.Second
	defaultAPIBaseURL     = "https://api.github.com"
	githubAcceptV3        = "application/vnd.github+json"
	githubAPIVersion      = "2022-11-28"
	ciBarWidth            = 79
	// ciBarMarkerLen is the length of the "-=O=-" bar marker. barPos is
	// clamped to [0, w-ciBarMarkerLen-1] so the marker fits within the line.
	ciBarMarkerLen  = 5
	ciRefreshPeriod = 100 * time.Millisecond
	ciWaveSineSteps = 200
)

// tempFile is the subset of *os.File used by downloadOnce. Allows tests to
// inject errors for Sync and Close without touching the real syscall.
type tempFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// osTempFile is called to create the download temp file. Overridden in tests
// to inject a tempFile that returns errors on Sync or Close.
var osTempFile = func(dir, pattern string) (tempFile, error) {
	return os.CreateTemp(dir, pattern)
}

var ciSinusTable = func() [ciWaveSineSteps]int {
	var t [ciWaveSineSteps]int
	for i := 0; i < ciWaveSineSteps; i++ {
		v := math.Sin(float64(i) / float64(ciWaveSineSteps) * 2 * math.Pi)
		t[i] = int(v*500000 + 500000)
	}
	return t
}()

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// FetchGitHubRelease downloads a single named asset from a GitHub release.
// It returns the absolute path to the local file. Retries, low-speed watchdog
// and sha256 verification (when the API exposes it) are applied automatically.
// The AssetName must match exactly; no substitutions are performed.
func FetchGitHubRelease(ctx context.Context, opts FetchOptions) (string, error) {
	if opts.Repo == "" {
		return "", errors.New(`repo required ("owner/name")`)
	}
	if !strings.Contains(opts.Repo, "/") {
		return "", fmt.Errorf(`repo must be "owner/name", got %q`, opts.Repo)
	}
	if opts.AssetName == "" {
		return "", errors.New("AssetName required")
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0o755
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	if opts.StallTimeout == 0 {
		opts.StallTimeout = defaultStallTimeout
	}
	if opts.StallLimit == 0 {
		opts.StallLimit = defaultStallLimit
	}
	if opts.RetryMax == 0 {
		opts.RetryMax = defaultRetryMax
	}
	if opts.RetrySleepMax == 0 {
		opts.RetrySleepMax = defaultRetrySleepMax
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = defaultAPIBaseURL
	} else {
		opts.APIBaseURL = strings.TrimRight(opts.APIBaseURL, "/")
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = newHTTPClient(opts.ConnectTimeout)
	}
	destDir := opts.DestDir
	if destDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		destDir = wd
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	dest := filepath.Join(destDir, opts.AssetName)
	verify := !opts.SkipVerify
	asset, err := fetchAssetWithRetry(ctx, &opts)
	if err != nil {
		if opts.FallbackToExisting && fileExists(dest) {
			fmt.Fprintf(
				opts.Stderr, "==> %s: API unavailable, using existing binary\n",
				opts.AssetName,
			)
			return dest, nil
		}
		return "", err
	}
	remoteDigest := parseDigest(asset.Digest)
	if verify && remoteDigest != "" {
		if fileExists(dest) {
			local, err := sha256File(dest)
			if err == nil && local == remoteDigest {
				fmt.Fprintf(
					opts.Stdout, "==> %s: up to date, skipping download\n",
					opts.AssetName,
				)
				return dest, nil
			}
		}
	}
	if err := downloadWithRetry(ctx, &opts, asset, dest, remoteDigest); err != nil {
		if opts.FallbackToExisting && fileExists(dest) {
			fmt.Fprintf(
				opts.Stderr, "==> %s: download failed, using existing binary\n",
				opts.AssetName,
			)
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

func fetchAssetWithRetry(ctx context.Context, opts *FetchOptions) (*ghAsset, error) {
	var asset *ghAsset
	err := retry(ctx, opts, "API", func(ctx context.Context) error {
		a, err := fetchAsset(ctx, opts)
		if err != nil {
			return err
		}
		asset = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fetch release metadata: %w", err)
	}
	return asset, nil
}

func setGitHubHeaders(req *http.Request, accept, token string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func fetchAsset(ctx context.Context, opts *FetchOptions) (*ghAsset, error) {
	var url string
	if opts.Tag == "" {
		url = fmt.Sprintf(
			"%s/repos/%s/releases/latest", opts.APIBaseURL, opts.Repo,
		)
	} else {
		url = fmt.Sprintf(
			"%s/repos/%s/releases/tags/%s", opts.APIBaseURL, opts.Repo, opts.Tag,
		)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setGitHubHeaders(req, githubAcceptV3, opts.Token)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf(
			"GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)),
		)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release payload: %w", err)
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == opts.AssetName {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf(
		"asset %q not found in release %s of %s",
		opts.AssetName, tagLabel(opts.Tag), opts.Repo,
	)
}

func downloadWithRetry(
	ctx context.Context, opts *FetchOptions, asset *ghAsset, dest, wantDigest string,
) error {
	return retry(ctx, opts, "download", func(ctx context.Context) error {
		return downloadOnce(ctx, opts, asset, dest, wantDigest)
	})
}

func downloadOnce(
	ctx context.Context, opts *FetchOptions, asset *ghAsset, dest, wantDigest string,
) error {
	url := asset.URL
	if url == "" {
		return errors.New("empty asset URL in release payload")
	}
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := &atomic.Bool{}
	// Start the CI progress renderer before the HTTP request so that the
	// connect and TLS handshake phases are covered by the wave animation.
	var ciR *ciRenderer
	if !opts.NoProgressBar && useCIProgress(opts) {
		ciR = newCIRenderer(opts.Stderr, -1)
		ciR.Start(dlCtx)
		defer ciR.Stop()
	}
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	setGitHubHeaders(req, "application/octet-stream", opts.Token)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)),
		)
	}
	tmp, err := osTempFile(filepath.Dir(dest), "."+opts.AssetName+".*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	total := resp.ContentLength
	if ciR != nil {
		ciR.SetTotal(total)
	}
	var counter atomic.Int64
	src := &countingReader{r: resp.Body, onRead: func(n int64) {
		counter.Add(n)
		if ciR != nil {
			ciR.AddBytes(n)
		}
	}}
	if opts.StallTimeout > 0 {
		go watchdog(
			dlCtx, cancel, stalled, &counter, opts.StallTimeout, opts.StallLimit,
			opts.Stderr, opts.AssetName,
		)
	}
	var hasher hash.Hash
	var writer io.Writer = tmp
	if wantDigest != "" {
		hasher = sha256.New()
		writer = io.MultiWriter(tmp, hasher)
	}
	if err := copyWithProgress(dlCtx, opts, writer, src, total, ciR); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stalled.Load() {
			return fmt.Errorf("transfer stalled: %w", err)
		}
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if wantDigest != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != wantDigest {
			return fmt.Errorf(
				"digest mismatch: expected %s, received %s", wantDigest, got,
			)
		}
	}
	if err := os.Chmod(tmpPath, opts.FileMode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return err
	}
	cleanupTmp = false
	return nil
}

func copyWithProgress(
	ctx context.Context, opts *FetchOptions, dst io.Writer, src io.Reader,
	total int64, ciR *ciRenderer,
) error {
	if opts.NoProgressBar || ciR != nil {
		_, err := io.Copy(dst, src)
		return err
	}
	return copyWithMpb(ctx, opts, dst, src, total)
}

func useCIProgress(opts *FetchOptions) bool {
	if opts.CIMode != nil {
		return *opts.CIMode
	}
	f, ok := opts.Stderr.(*os.File)
	if !ok {
		return true
	}
	return !term.IsTerminal(int(f.Fd()))
}

func copyWithMpb(
	ctx context.Context, opts *FetchOptions, dst io.Writer, src io.Reader, total int64,
) error {
	p := mpb.NewWithContext(
		ctx,
		mpb.WithOutput(opts.Stderr),
		mpb.WithRefreshRate(150*time.Millisecond),
	)
	bar := p.New(
		total,
		mpb.BarStyle().Rbound("|"),
		mpb.PrependDecorators(
			decor.Name(opts.AssetName+" "),
			decor.CountersKibiByte("% .2f / % .2f"),
		),
		mpb.AppendDecorators(
			decor.EwmaSpeed(decor.SizeB1024(0), " % .2f", 60),
			decor.Name(" "),
			decor.EwmaETA(decor.ET_STYLE_GO, 60),
		),
	)
	proxied := bar.ProxyReader(src)
	_, err := io.Copy(dst, proxied)
	proxied.Close()
	if err != nil {
		bar.Abort(false)
	} else {
		bar.SetTotal(-1, true)
	}
	p.Wait()
	return err
}

// ciRenderer prints a progress bar with one frame per line, so that CI log
// viewers that do not honor carriage returns keep every frame in the log as
// a vertical trail.
//
// While no bytes have arrived yet it prints wave frames (a moving "-=O=-"
// marker plus four "#" characters walking along a sine curve), matching the
// look of curl's fly() in tool_cb_prg.c. Once the first byte is received it
// switches to a pv-style line like:
//
//	3.0M/20.0M 0:01:06 [18.6K/s] [====>-----------] 30% ETA 0:05:58
//
// A new line is printed whenever the progress bar advances by at least one
// character or after ciRefreshPeriod, whichever comes first.
type ciRenderer struct {
	out       io.Writer
	barWidth  int // line width; defaults to ciBarWidth when zero
	total     atomic.Int64
	counter   atomic.Int64
	stop      chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	startedAt time.Time
	tick      int
	barPos    int
	barMove   int
	lastBar   int
	lastPct   float64
	lastPrint time.Time
	// EWMA speed state, bytes per second.
	speedEWMA   float64
	speedLastAt time.Time
	speedLastN  int64
	started     bool
}

func newCIRenderer(out io.Writer, total int64) *ciRenderer {
	r := &ciRenderer{
		out:     out,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		barMove: 1,
		tick:    150,
		lastPct: -1,
	}
	r.total.Store(total)
	return r
}

// SetTotal updates the expected transfer size once known.
func (r *ciRenderer) SetTotal(total int64) {
	r.total.Store(total)
}

func (r *ciRenderer) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.startedAt = time.Now()
	r.mu.Unlock()
	go r.loop(ctx)
}

func (r *ciRenderer) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	total := r.total.Load()
	if total > 0 && r.lastBar > 0 {
		final := r.renderHashLine(r.counter.Load(), total)
		fmt.Fprintln(r.out, final)
	}
}

// AddBytes is called by the counting reader on every Read.
func (r *ciRenderer) AddBytes(n int64) {
	if n > 0 {
		r.counter.Add(n)
	}
}

func (r *ciRenderer) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(ciRefreshPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-ticker.C:
			r.frame()
		}
	}
}

func (r *ciRenderer) frame() {
	cur := r.counter.Load()
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur == 0 {
		fmt.Fprintln(r.out, r.renderWaveLine(true))
		return
	}
	r.updateSpeed(cur, now)
	total := r.total.Load()
	if total <= 0 {
		fmt.Fprintln(r.out, r.renderWaveLineWithBytes(cur))
		return
	}
	r.printPVIfChanged(cur, total, now)
}

// updateSpeed maintains an EWMA of the throughput in bytes per second with a
// ~1 second time constant, matching pv -r behavior.
func (r *ciRenderer) updateSpeed(cur int64, now time.Time) {
	if r.speedLastAt.IsZero() {
		r.speedLastAt = now
		r.speedLastN = cur
		return
	}
	dt := now.Sub(r.speedLastAt).Seconds()
	if dt <= 0 {
		return
	}
	inst := float64(cur-r.speedLastN) / dt
	if inst < 0 {
		inst = 0
	}
	const tau = 1.0
	alpha := 1 - math.Exp(-dt/tau)
	if r.speedEWMA == 0 {
		r.speedEWMA = inst
	} else {
		r.speedEWMA = alpha*inst + (1-alpha)*r.speedEWMA
	}
	r.speedLastAt = now
	r.speedLastN = cur
}

func (r *ciRenderer) printPVIfChanged(cur, total int64, now time.Time) {
	line, bar, pct := r.pvLine(cur, total, now)
	sinceLast := now.Sub(r.lastPrint)
	if r.lastPrint.IsZero() || bar != r.lastBar || sinceLast >= time.Second {
		fmt.Fprintln(r.out, line)
		r.lastBar = bar
		r.lastPct = pct
		r.lastPrint = now
	}
}

func (r *ciRenderer) renderHashLine(cur, total int64) string {
	line, _, _ := r.pvLine(cur, total, time.Now())
	return line
}

// pvLine renders a pv-style progress line:
//
//	3.0M/20.0M 0:01:06 [18.6K/s] [====>-----------] 30% ETA 0:05:58
//
// It returns the line, the number of filled bar cells (used for update
// throttling) and the percent value.
func (r *ciRenderer) pvLine(cur, total int64, now time.Time) (string, int, float64) {
	if cur > total {
		cur = total
	}
	frac := float64(cur) / float64(total)
	pct := frac * 100.0
	elapsed := now.Sub(r.startedAt)
	var eta time.Duration
	if r.speedEWMA > 1 && cur < total {
		eta = time.Duration(float64(total-cur)/r.speedEWMA) * time.Second
	}
	prefix := fmt.Sprintf(
		"%s/%s %s [%s/s] ",
		humanBytes(cur, true), humanBytes(total, true),
		formatDuration(elapsed), humanBytes(int64(r.speedEWMA), true),
	)
	suffix := fmt.Sprintf(" %3.0f%% ETA %s", pct, formatDuration(eta))
	if cur >= total {
		suffix = fmt.Sprintf(" %3.0f%%", pct)
	}
	barw := r.lineWidth() - len(prefix) - len(suffix) - 2
	if barw < 4 {
		barw = 4
	}
	filled := int(float64(barw) * frac)
	bar := make([]byte, barw)
	for i := 0; i < filled; i++ {
		bar[i] = '='
	}
	head := filled
	if head < barw && cur < total {
		bar[head] = '>'
		head++
	}
	for i := head; i < barw; i++ {
		bar[i] = '-'
	}
	return prefix + "[" + string(bar) + "]" + suffix, filled, pct
}

// humanBytes renders a byte count as a human-readable string. When short is
// true the output uses compact pv-style single-letter suffixes without spaces
// (e.g. "3.0M"). When short is false it uses IEC binary suffixes with spaces
// (e.g. "3.0 MiB"), as used by curl's wave-line display.
func humanBytes(n int64, short bool) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	const suffixes = "KMGTPE"
	if n < unit {
		if short {
			return fmt.Sprintf("%dB", n)
		}
		return fmt.Sprintf("%d B", n)
	}
	f, idx := float64(n)/unit, 0
	for f >= unit && idx < len(suffixes)-1 {
		f /= unit
		idx++
	}
	if short {
		if f >= 100 {
			return fmt.Sprintf("%.0f%c", f, suffixes[idx])
		}
		return fmt.Sprintf("%.1f%c", f, suffixes[idx])
	}
	return fmt.Sprintf("%.1f %ciB", f, suffixes[idx])
}

// formatDuration renders a duration as h:mm:ss (or m:ss for shorter ones),
// matching pv output like "0:01:06" and "0:05:58".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

func (r *ciRenderer) lineWidth() int {
	if r.barWidth > 0 {
		return r.barWidth
	}
	return ciBarWidth
}

// renderWaveLine mirrors the fly() function from curl's tool_cb_prg.c. The
// "-=O=-" marker walks left-to-right, then right-to-left, while four "#"
// characters ride four sine waves shifted by 5 ticks each. The tick counter
// is advanced by 2 per frame, exactly like curl.
func (r *ciRenderer) renderWaveLine(advance bool) string {
	w := r.lineWidth()
	buf := make([]byte, w)
	for i := 0; i < w; i++ {
		buf[i] = ' '
	}
	check := w - 2
	if r.barPos+ciBarMarkerLen <= w {
		copy(buf[r.barPos:], "-=O=-")
	}
	if check > 0 {
		for _, shift := range []int{0, 5, 10, 15} {
			pos := ciSinusTable[(r.tick+shift)%ciWaveSineSteps]/(1000000/check) + 1
			if pos >= 0 && pos < w {
				buf[pos] = '#'
			}
		}
	}
	if advance {
		r.tick += 2
		if r.tick >= ciWaveSineSteps {
			r.tick -= ciWaveSineSteps
		}
		r.barPos += r.barMove
		if r.barPos >= w-ciBarMarkerLen-1 {
			r.barMove = -1
			r.barPos = w - ciBarMarkerLen - 1
		} else if r.barPos < 0 {
			r.barMove = 1
			r.barPos = 0
		}
	}
	return string(buf)
}

func (r *ciRenderer) renderWaveLineWithBytes(cur int64) string {
	line := []byte(r.renderWaveLine(true))
	// Right-align the human-readable byte count inside the fixed-width buffer
	// by overwriting the trailing bytes, preserving a single space separator.
	suffix := humanBytes(cur, false)
	pos := len(line) - len(suffix)
	if pos > 0 {
		line[pos-1] = ' '
		copy(line[pos:], suffix)
	}
	return string(line)
}

func watchdog(
	ctx context.Context, cancel context.CancelFunc, stalled *atomic.Bool,
	counter *atomic.Int64, window time.Duration, limit int64, stderr io.Writer,
	name string,
) {
	ticker := time.NewTicker(window)
	defer ticker.Stop()
	prev := counter.Load()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := counter.Load()
			if cur-prev < limit {
				fmt.Fprintf(
					stderr,
					"==> %s: transfer stalled (<%d B in %s), aborting\n",
					name, limit, window,
				)
				stalled.Store(true)
				cancel()
				return
			}
			prev = cur
		}
	}
}

type countingReader struct {
	r      io.Reader
	onRead func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.onRead(int64(n))
	}
	return n, err
}

func retry(
	ctx context.Context, opts *FetchOptions, action string,
	fn func(context.Context) error,
) error {
	var lastErr error
	sleep := time.Second
	if sleep > opts.RetrySleepMax {
		sleep = opts.RetrySleepMax
	}
	for attempt := 1; attempt <= opts.RetryMax; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt == opts.RetryMax {
			break
		}
		fmt.Fprintf(
			opts.Stderr,
			"==> %s: %s failed: %v, retry %d/%d, sleep %s\n",
			opts.AssetName, action, lastErr, attempt, opts.RetryMax, sleep,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
		next := sleep * 2
		if next > opts.RetrySleepMax {
			next = opts.RetrySleepMax
		}
		sleep = next
	}
	return lastErr
}

func parseDigest(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "sha256:") {
		return strings.TrimPrefix(raw, "sha256:")
	}
	return ""
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func tagLabel(tag string) string {
	if tag == "" {
		return "latest"
	}
	return tag
}

func newHTTPClient(connectTimeout time.Duration) *http.Client {
	defaultTr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		defaultTr = &http.Transport{}
	}
	tr := defaultTr.Clone()
	tr.ResponseHeaderTimeout = connectTimeout
	tr.TLSHandshakeTimeout = connectTimeout
	tr.IdleConnTimeout = 30 * time.Second
	dialer := &net.Dialer{
		Timeout:   connectTimeout,
		KeepAlive: 30 * time.Second,
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{Transport: tr}
}
