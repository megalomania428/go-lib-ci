// Package ci is documented in doc.go.
// cspell:ignore Ewma Kibi Rbound TCGETS vbauerster badurl copyerr defaultdest isdir
// cspell:ignore rawhex stallsig syncfail barwidth barmove noprogress barw KMGTPE
// cspell:ignore unparseable alives
package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

// DownloadOptions controls FetchURL. It carries the transport-agnostic knobs
// shared by every download: retries, low-speed watchdog, progress rendering
// and output sinks. Higher-level helpers such as FetchGitHubRelease build a
// DownloadOptions from their own configuration.
type DownloadOptions struct {
	// Name labels the transfer in progress bars, temp files and log lines.
	// When empty it falls back to the destination base name.
	Name string
	// FileMode is applied to the downloaded file. Defaults to 0o755.
	FileMode os.FileMode
	// WantDigest is the expected lowercase hex sha256 of the payload. When
	// non-empty the downloaded file is verified and a mismatch causes an
	// error.
	WantDigest string
	// SetHeaders decorates the download request before it is sent. It may be
	// nil. Used to inject authentication or Accept headers.
	SetHeaders func(*http.Request)
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
	// BackoffOptions carries the retry/backoff schedule knobs shared with
	// RetryOptions and FetchOptions.
	BackoffOptions
	// HTTPClient overrides the default *http.Client.
	HTTPClient *http.Client
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
	defaultConnectTimeout      = 11 * time.Second
	defaultStallTimeout        = 30 * time.Second
	defaultStallLimit          = int64(1)
	defaultMaxRetriesTime      = 111 * time.Second
	defaultBackoffInitial      = 1 * time.Second
	defaultBackoffStep         = 1 * time.Second
	defaultBackoffStepAttempts = 3
	defaultBackoffCap          = 7 * time.Second
	ciBarWidth                 = 79
	// ciBarMarkerLen is the length of the "-=O=-" bar marker. barPos is
	// clamped to [0, w-ciBarMarkerLen-1] so the marker fits within the line.
	ciBarMarkerLen  = 5
	ciRefreshPeriod = 100 * time.Millisecond
	ciWaveSineSteps = 200
)

// BackoffOptions carries the retry/backoff schedule knobs shared by
// DownloadOptions, RetryOptions and FetchOptions. It is embedded into each of
// them so the documentation and defaulting live in a single place.
type BackoffOptions struct {
	// MaxRetriesTime is the total wall-clock budget the retry/backoff loop
	// may spend sleeping between attempts before giving up. Zero keeps the
	// default of 111 seconds; a negative value disables retries entirely.
	MaxRetriesTime time.Duration
	// BackoffInitial is the first backoff delay. Zero keeps the default of
	// 1 second.
	BackoffInitial time.Duration
	// BackoffStep is added to the delay after every BackoffStepAttempts
	// attempts. Zero keeps the default of 1 second.
	BackoffStep time.Duration
	// BackoffStepAttempts is the number of attempts in each backoff block:
	// the delay grows by BackoffStep once every BackoffStepAttempts
	// attempts. Zero keeps the default of 3.
	BackoffStepAttempts int
	// BackoffCap caps a single backoff delay; once reached, the schedule
	// resets to BackoffInitial. Zero keeps the default of 7 seconds.
	BackoffCap time.Duration
}

// applyBackoffDefaults fills the zero-value fields of b with their documented
// defaults. It is safe to call more than once.
func applyBackoffDefaults(b *BackoffOptions) {
	if b.MaxRetriesTime == 0 {
		b.MaxRetriesTime = defaultMaxRetriesTime
	}
	if b.BackoffInitial <= 0 {
		b.BackoffInitial = defaultBackoffInitial
	}
	if b.BackoffStep <= 0 {
		b.BackoffStep = defaultBackoffStep
	}
	if b.BackoffStepAttempts <= 0 {
		b.BackoffStepAttempts = defaultBackoffStepAttempts
	}
	if b.BackoffCap <= 0 {
		b.BackoffCap = defaultBackoffCap
	}
}

// Exported retry/backoff defaults so downstream tools can render the same
// values in their help output without duplicating the magic numbers.
const (
	// DefaultMaxRetriesTime is the default retry/backoff wall-clock budget.
	DefaultMaxRetriesTime = defaultMaxRetriesTime
	// DefaultBackoffInitial is the default first backoff delay.
	DefaultBackoffInitial = defaultBackoffInitial
	// DefaultBackoffStep is the default per-block backoff increment.
	DefaultBackoffStep = defaultBackoffStep
	// DefaultBackoffStepAttempts is the default number of attempts per block.
	DefaultBackoffStepAttempts = defaultBackoffStepAttempts
	// DefaultBackoffCap is the default single-attempt backoff cap.
	DefaultBackoffCap = defaultBackoffCap
)

// tempFile is the subset of *os.File used by downloadOnce. Allows tests to
// inject errors for Sync and Close without touching the real syscall.
type tempFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// osOpenPart is called to open the deterministic partial-download file. When
// truncate is true the file is created or truncated to zero (a fresh download
// or a server that ignored the Range request); otherwise it is opened for
// append so a retry continues from the already downloaded prefix. Overridden
// in tests to inject a tempFile that returns errors on Sync or Close.
var osOpenPart = func(name string, truncate bool) (tempFile, error) {
	flag := os.O_WRONLY | os.O_CREATE
	if truncate {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_APPEND
	}
	return os.OpenFile(name, flag, 0o644)
}

var ciSinusTable = func() [ciWaveSineSteps]int {
	var t [ciWaveSineSteps]int
	for i := 0; i < ciWaveSineSteps; i++ {
		v := math.Sin(float64(i) / float64(ciWaveSineSteps) * 2 * math.Pi)
		t[i] = int(v*500000 + 500000)
	}
	return t
}()

// applyDownloadDefaults fills the zero-value fields of opts with their
// documented defaults. It is safe to call more than once.
func applyDownloadDefaults(opts *DownloadOptions) {
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
	applyBackoffDefaults(&opts.BackoffOptions)
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = NewHTTPClient(opts.ConnectTimeout)
	}
}

// FetchURL downloads a single file from an http/https URL to dest with the
// same retries, low-speed watchdog, sha256 verification and curl-like
// progress bar used for GitHub release assets. The parent directory of dest
// must already exist. It returns dest on success.
func FetchURL(ctx context.Context, url, dest string, opts DownloadOptions) (string,
	error) {
	if url == "" {
		return "", errors.New("url required")
	}
	if dest == "" {
		return "", errors.New("dest required")
	}
	applyDownloadDefaults(&opts)
	if opts.Name == "" {
		opts.Name = filepath.Base(dest)
	}
	if err := downloadWithRetry(ctx, &opts, url, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func downloadWithRetry(
	ctx context.Context, opts *DownloadOptions, url, dest string,
) error {
	// Compute the deterministic partial-file path once, outside the retry
	// loop, so every attempt works on the same file and a retry can resume
	// from the bytes an earlier attempt already downloaded. Retry only sees
	// the closure, so the path must be fixed here rather than in downloadOnce.
	part := partPath(opts, dest)
	return retry(ctx, opts, "download", func(ctx context.Context) error {
		return downloadOnce(ctx, opts, url, dest, part)
	})
}

// partPath returns the deterministic partial-download file path next to dest.
// It is stable across retry attempts so a resumed download reuses the bytes
// already fetched by an earlier attempt.
func partPath(opts *DownloadOptions, dest string) string {
	name := opts.Name
	if name == "" {
		name = filepath.Base(dest)
	}
	return filepath.Join(filepath.Dir(dest), "."+name+".part")
}

func downloadOnce(
	ctx context.Context, opts *DownloadOptions, url, dest, part string,
) error {
	if url == "" {
		return errors.New("empty download URL")
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
	// A previous attempt may have left a partial file; ask the server for the
	// remaining bytes via Range so the transfer resumes instead of restarting.
	existing := partialSize(part)
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if opts.SetHeaders != nil {
		opts.SetHeaders(req)
	}
	if existing > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existing))
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// The partial file is already at least as long as the target (usually
		// a leftover from a completed or corrupted attempt). Drop it so the
		// next attempt starts clean.
		_ = os.Remove(part)
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)),
		)
	}
	// Resume only when we asked for a range and the server honored it with
	// 206; a 200 means the server ignored Range and streams the whole file,
	// so the stale prefix must be discarded to avoid a corrupt result.
	resume := existing > 0 && resp.StatusCode == http.StatusPartialContent
	total := resolveTotal(resp, existing, resume)
	tmp, err := osOpenPart(part, !resume)
	if err != nil {
		return fmt.Errorf("open partial file: %w", err)
	}
	closed := false
	closeTmp := func() error {
		if closed {
			return nil
		}
		closed = true
		return tmp.Close()
	}
	removePart := false
	defer func() {
		_ = closeTmp()
		if removePart {
			_ = os.Remove(part)
		}
	}()
	if ciR != nil {
		ciR.SetTotal(total)
		if resume {
			ciR.AddBytes(existing)
		}
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
			opts.Stderr, opts.Name,
		)
	}
	if err := copyWithProgress(dlCtx, opts, tmp, src, total, ciR); err != nil {
		// Keep the partial file on transfer errors and stalls: preserving it
		// is exactly what lets the next attempt resume from where this left.
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
	if err := closeTmp(); err != nil {
		return err
	}
	if opts.WantDigest != "" {
		// Hash the finished file in a single pass rather than streaming while
		// writing: this stays correct whether the file was resumed or fetched
		// from scratch, since the streamed hasher would miss the prefix.
		got, err := sha256File(part)
		if err != nil {
			return err
		}
		if got != opts.WantDigest {
			removePart = true
			return fmt.Errorf(
				"digest mismatch: expected %s, received %s", opts.WantDigest, got,
			)
		}
	}
	if err := os.Chmod(part, opts.FileMode); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return err
	}
	return nil
}

// partialSize returns the size of an existing partial file, or zero when it is
// absent, a directory or otherwise unusable as a resume source.
func partialSize(part string) int64 {
	fi, err := os.Stat(part)
	if err != nil || fi.IsDir() {
		return 0
	}
	return fi.Size()
}

// resolveTotal returns the full expected size for progress reporting. For a
// resumed 206 response it prefers the total from Content-Range and otherwise
// adds the already downloaded bytes to the remaining Content-Length.
func resolveTotal(resp *http.Response, existing int64, resume bool) int64 {
	total := resp.ContentLength
	if !resume {
		return total
	}
	if ct := parseContentRangeTotal(resp.Header.Get("Content-Range")); ct > 0 {
		return ct
	}
	if total >= 0 {
		return total + existing
	}
	return total
}

// parseContentRangeTotal extracts the total size from a Content-Range header of
// the form "bytes <start>-<end>/<total>". It returns zero when the total is
// missing or unparseable (e.g. "bytes 0-1/*").
func parseContentRangeTotal(header string) int64 {
	slash := strings.LastIndex(header, "/")
	if slash < 0 {
		return 0
	}
	total, err := strconv.ParseInt(strings.TrimSpace(header[slash+1:]), 10, 64)
	if err != nil {
		return 0
	}
	return total
}

func copyWithProgress(
	ctx context.Context, opts *DownloadOptions, dst io.Writer, src io.Reader,
	total int64, ciR *ciRenderer,
) error {
	if opts.NoProgressBar || ciR != nil {
		_, err := io.Copy(dst, src)
		return err
	}
	return copyWithMpb(ctx, opts, dst, src, total)
}

func useCIProgress(opts *DownloadOptions) bool {
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
	ctx context.Context, opts *DownloadOptions, dst io.Writer, src io.Reader, total int64,
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
			decor.Name(opts.Name+" "),
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
	cur := r.counter.Load()
	if total > 0 && cur > 0 {
		final := r.renderHashLine(cur, total)
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
	line, bar, _ := r.pvLine(cur, total, now)
	sinceLast := now.Sub(r.lastPrint)
	if r.lastPrint.IsZero() || bar != r.lastBar || sinceLast >= time.Second {
		fmt.Fprintln(r.out, line)
		r.lastBar = bar
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

// formatDuration renders a duration as h:mm:ss, matching pv output like
// "0:01:06" and "0:05:58".
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
	if r.barPos >= 0 && r.barPos+ciBarMarkerLen <= w {
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

// closeIdler is implemented by *http.Transport and lets Retry drop pooled
// idle connections between attempts, so a retry after a request that hung
// awaiting response headers re-dials instead of reusing the dead connection.
type closeIdler interface {
	CloseIdleConnections()
}

// RetryOptions controls Retry. Zero values fall back to the same defaults
// FetchURL and FetchGitHubRelease use today.
type RetryOptions struct {
	// Name labels the operation in retry log lines.
	Name string
	// BackoffOptions carries the retry/backoff schedule knobs shared with
	// DownloadOptions and FetchOptions.
	BackoffOptions
	// Stderr receives retry warnings. Defaults to os.Stderr.
	Stderr io.Writer
	// IdleCloser, when non-nil, has its idle connections closed after each
	// failed attempt so the next attempt re-dials. *http.Transport satisfies
	// this. Zero value keeps the previous behavior.
	IdleCloser closeIdler
}

// applyRetryDefaults fills the zero-value fields of opts with their documented
// defaults. It is safe to call more than once.
func applyRetryDefaults(opts *RetryOptions) {
	applyBackoffDefaults(&opts.BackoffOptions)
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
}

// backoffDelay returns the sleep duration for the given attempt (0-indexed).
// The schedule rises by BackoffStep every BackoffStepAttempts attempts up to
// BackoffCap, then resets to BackoffInitial — giving short bursts at each
// level before backing off further. It mirrors the algorithm from the
// ai-review retry-backoff patch.
func backoffDelay(attempt int, opts *RetryOptions) time.Duration {
	step := opts.BackoffStep
	block := opts.BackoffStepAttempts
	// capBlocks is the number of rising blocks needed to reach the cap; the
	// full cycle adds one extra block of cap-level attempts before resetting.
	capBlocks := int(opts.BackoffCap / step)
	cycle := (capBlocks + 1) * block
	delay := time.Duration((attempt%cycle)/block)*step + opts.BackoffInitial
	if delay > opts.BackoffCap {
		delay = opts.BackoffCap
	}
	return delay
}

// Retry runs fn, retrying on error with the block-wise backoff schedule
// (see backoffDelay) until it succeeds, ctx is done, or the cumulative
// backoff sleep would exceed MaxRetriesTime.
func Retry(
	ctx context.Context, opts RetryOptions, fn func(context.Context) error,
) error {
	applyRetryDefaults(&opts)
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr := fn(ctx)
		if lastErr == nil {
			return nil
		}
		delay := backoffDelay(attempt, &opts)
		// A non-positive budget disables retries; otherwise stop once the
		// next sleep would push the cumulative wait past the budget.
		if opts.MaxRetriesTime <= 0 || waited+delay > opts.MaxRetriesTime {
			return lastErr
		}
		// Drop pooled idle connections so the next attempt re-dials instead
		// of reusing a connection left dead by a request that hung awaiting
		// response headers.
		if opts.IdleCloser != nil {
			opts.IdleCloser.CloseIdleConnections()
		}
		fmt.Fprintf(
			opts.Stderr,
			"==> %s failed: %v, sleep %s, elapsed %s of %s\n",
			opts.Name, lastErr, delay, waited, opts.MaxRetriesTime,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		waited += delay
	}
}

// retry adapts the download-specific DownloadOptions to the exported Retry
// building block, preserving the historical "==> {Name}: {action} failed"
// log format used by FetchURL and FetchGitHubRelease.
func retry(
	ctx context.Context, opts *DownloadOptions, action string,
	fn func(context.Context) error,
) error {
	return Retry(ctx, RetryOptions{
		Name:           opts.Name + ": " + action,
		BackoffOptions: opts.BackoffOptions,
		Stderr:         opts.Stderr,
		IdleCloser:     transportIdleCloser(opts.HTTPClient),
	}, fn)
}

// transportIdleCloser returns the client's transport as a closeIdler when it
// implements CloseIdleConnections (as *http.Transport does), so Retry can drop
// pooled idle connections between attempts. It returns nil otherwise, which
// keeps the previous behavior.
func transportIdleCloser(client *http.Client) closeIdler {
	if client == nil {
		return nil
	}
	if c, ok := client.Transport.(closeIdler); ok {
		return c
	}
	return nil
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

// NewHTTPClient builds an *http.Client tuned for the same reliability
// primitives FetchURL and FetchGitHubRelease use: it clones
// http.DefaultTransport, sets ResponseHeaderTimeout and TLSHandshakeTimeout to
// connectTimeout, and dials with a matching net.Dialer. Consumers that need a
// CheckRedirect override clone the returned
// client's Transport and set their own CheckRedirect.
//
// Keep-alive is deliberately disabled: every request opens a fresh connection
// (and a fresh proxy CONNECT). The library's network calls are sequential with
// backoff between attempts, so connection reuse buys nothing; worse, a request
// that hangs awaiting response headers leaves the dead connection in the idle
// pool, and the next Retry attempt would reuse it instead of re-dialing.
// Disabling keep-alive forces the retry to rebuild the connection.
func NewHTTPClient(connectTimeout time.Duration) *http.Client {
	defaultTr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		defaultTr = &http.Transport{}
	}
	tr := defaultTr.Clone()
	tr.ResponseHeaderTimeout = connectTimeout
	tr.TLSHandshakeTimeout = connectTimeout
	dialer := &net.Dialer{
		Timeout: connectTimeout,
	}
	tr.DialContext = dialer.DialContext
	tr.DisableKeepAlives = true
	return &http.Client{Transport: tr}
}
