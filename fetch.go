// Package ci is documented in doc.go.
// cspell:ignore badurl copyerr defaultdest isdir alives noprogress
// cspell:ignore rawhex stallsig syncfail
package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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
) (retErr error) {
	if url == "" {
		return errors.New("empty download URL")
	}
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := &atomic.Bool{}
	// Single progress bar for the whole attempt: created before the HTTP
	// request so wave/indeterminate animation covers connect and TLS, then
	// reconfigured with the real total once the response arrives.
	bar := NewProgressBar(dlCtx, ProgressOptions{
		Name:          opts.Name,
		Total:         -1,
		Stderr:        opts.Stderr,
		CIMode:        opts.CIMode,
		NoProgressBar: opts.NoProgressBar,
	})
	defer func() { bar.Finish(retErr) }()

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
	bar.SetTotal(total)
	if resume {
		bar.AddBytes(existing)
	}
	var counter atomic.Int64
	src := &countingReader{r: resp.Body, onRead: func(n int64) {
		counter.Add(n)
	}}
	if opts.StallTimeout > 0 {
		go watchdog(
			dlCtx, cancel, stalled, &counter, opts.StallTimeout, opts.StallLimit,
			opts.Stderr, opts.Name,
		)
	}
	wrapped := bar.WrapReader(src)
	_, copyErr := io.Copy(tmp, wrapped)
	_ = wrapped.Close()
	if copyErr != nil {
		// Keep the partial file on transfer errors and stalls: preserving it
		// is exactly what lets the next attempt resume from where this left.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stalled.Load() {
			return fmt.Errorf("transfer stalled: %w", copyErr)
		}
		return copyErr
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
