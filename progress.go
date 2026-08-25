// Package ci is documented in doc.go.
// cspell:ignore barw Ewma Kibi KMGTPE Rbound vbauerster
package ci

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

const (
	ciBarWidth = 79
	// ciBarMarkerLen is the length of the "-=O=-" bar marker. barPos is
	// clamped to [0, w-ciBarMarkerLen-1] so the marker fits within the line.
	ciBarMarkerLen  = 5
	ciRefreshPeriod = 100 * time.Millisecond
	ciWaveSineSteps = 200
	// defaultProgressRefresh is the mpb redraw period used as the zero-value
	// fallback for ProgressOptions.RefreshRate.
	defaultProgressRefresh = 150 * time.Millisecond
)

var ciSinusTable = func() [ciWaveSineSteps]int {
	var t [ciWaveSineSteps]int
	for i := 0; i < ciWaveSineSteps; i++ {
		v := math.Sin(float64(i) / float64(ciWaveSineSteps) * 2 * math.Pi)
		t[i] = int(v*500000 + 500000)
	}
	return t
}()

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
// character or once per second, whichever comes first.
type ciRenderer struct {
	out       io.Writer
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
	// lastPct is the last percentage actually printed by printPVIfChanged.
	// It is used so Stop does not duplicate a 100% line and does not draw a
	// lone 100% frame without any prior output.
	lastPct float64
	// finalPrinted is set by Stop after it has handled the optional final
	// line, so a second Stop is a no-op without faking lastPct.
	finalPrinted bool
	// EWMA speed state, bytes per second.
	speedEWMA   float64
	speedLastAt time.Time
	speedLastN  int64
	started     bool
	// reverse fills the bar tail-first instead of head-first, used for
	// upload progress so the direction matches bytes leaving to the network.
	reverse bool
	// stopped is set under mu when stop is closed, so concurrent Stop
	// calls do not race on close(r.stop).
	stopped bool
}

func newCIRenderer(out io.Writer, total int64, reverse bool) *ciRenderer {
	r := &ciRenderer{
		out:     out,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		barMove: 1,
		tick:    150,
		reverse: reverse,
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
	if !r.stopped {
		r.stopped = true
		close(r.stop)
	}
	r.mu.Unlock()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalPrinted {
		return
	}
	total := r.total.Load()
	cur := r.counter.Load()
	if total > 0 && cur > 0 &&
		!r.lastPrint.IsZero() && r.lastPct < 100 {
		final := r.renderFinalLine(cur, total)
		fmt.Fprintln(r.out, final)
	}
	r.finalPrinted = true
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
	changed := r.lastPrint.IsZero() || bar != r.lastBar || sinceLast >= time.Second
	if !changed {
		return
	}
	if pct >= 100 && (r.lastPrint.IsZero() || r.lastPct >= 100) {
		return
	}
	fmt.Fprintln(r.out, line)
	r.lastBar = bar
	r.lastPct = pct
	r.lastPrint = now
}

func (r *ciRenderer) renderFinalLine(cur, total int64) string {
	line, _, _ := r.pvLine(cur, total, time.Now())
	return line
}

// pvLine renders a pv-style progress line:
//
//	3.0M/20.0M 0:01:06 [18.6K/s] [====>-----------] 30% ETA 0:05:58
//
// It returns the line, the number of filled bar cells (used for update
// throttling) and the percent value. When r.reverse is true the filled
// region grows from the right edge of the bar instead of the left.
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
	barw := ciBarWidth - len(prefix) - len(suffix) - 2
	filled := int(float64(barw) * frac)
	bar := make([]byte, barw)
	if r.reverse {
		for i := 0; i < barw; i++ {
			bar[i] = '-'
		}
		for i := barw - filled; i < barw; i++ {
			bar[i] = '='
		}
		head := barw - filled - 1
		if head >= 0 && cur < total {
			bar[head] = '<'
		}
	} else {
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

// renderWaveLine mirrors the fly() function from curl's tool_cb_prg.c. The
// "-=O=-" marker walks left-to-right, then right-to-left, while four "#"
// characters ride four sine waves shifted by 5 ticks each. The tick counter
// is advanced by 2 per frame, exactly like curl.
func (r *ciRenderer) renderWaveLine(advance bool) string {
	w := ciBarWidth
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

// useCIRenderer decides whether the line-per-frame CI renderer should be used
// instead of the TTY-oriented mpb renderer. ciMode nil enables auto-detection
// based on whether stderr is a terminal; a non-nil value overrides it.
func useCIRenderer(stderr io.Writer, ciMode *bool) bool {
	if ciMode != nil {
		return *ciMode
	}
	f, ok := stderr.(*os.File)
	if !ok {
		return true
	}
	return !term.IsTerminal(int(f.Fd()))
}

// ProgressOptions configures ProgressBar behavior. Zero values reproduce the
// defaults FetchURL applies today for downloads.
type ProgressOptions struct {
	// Name is printed before the byte counter in mpb mode. An empty string
	// is fine: the bar is then rendered without a prefix.
	Name string
	// Total is the known full size of the stream in bytes. 0 or a negative
	// value leaves the size unknown: the CI renderer shows the wave
	// indicator and mpb an indeterminate bar, without percentages.
	Total int64
	// Stderr is where the bar is drawn. Defaults to os.Stderr.
	Stderr io.Writer
	// CIMode forces the line-per-frame CI renderer (true) or mpb (false).
	// nil means auto-detection via term.IsTerminal(Stderr), as in FetchURL.
	CIMode *bool
	// NoProgressBar disables rendering entirely.
	NoProgressBar bool
	// Reverse fills the bar visually right-to-left. Used for uploads so the
	// direction matches bytes "leaking" into the network. Has no effect on
	// the wave phase (when Total is unknown).
	Reverse bool
	// RefreshRate overrides the mpb redraw period only; the CI renderer
	// always refreshes at ciRefreshPeriod. <= 0 means defaultProgressRefresh.
	RefreshRate time.Duration
}

// ProgressBar is a progress bar controller living on top of an arbitrary
// stream. An instance is created by NewProgressBar and is no longer used
// after Finish.
type ProgressBar interface {
	// WrapReader returns an io.ReadCloser that adds the number of bytes read
	// to the bar's counter on every Read. Close closes the wrapped reader
	// too, if it implements io.Closer.
	WrapReader(io.Reader) io.ReadCloser
	// WrapWriter returns an io.WriteCloser that adds the number of bytes
	// written to the bar's counter on every Write. Unlike WrapReader,
	// Close never closes the wrapped writer, even if it implements
	// io.Closer: callers remain responsible for closing it themselves.
	WrapWriter(io.Writer) io.WriteCloser
	// AddBytes increments the counter directly. Useful when the bytes are
	// already counted by an existing pipeline.
	AddBytes(int64)
	// SetTotal updates the expected size after start (e.g. when
	// Content-Length arrives only after the bar has already been drawn).
	// Non-positive values mean the size is unknown: the CI renderer
	// switches to the wave animation, the mpb bar keeps its current total.
	SetTotal(int64)
	// Finish closes the bar, printing the final line (in CI mode) or
	// finishing mpb.Progress. err is passed so the mpb bar can render an
	// abort instead of a successful completion.
	Finish(err error)
}

// noopProgressBar is a ProgressBar implementation that renders nothing.
type noopProgressBar struct{}

type noopWriteCloser struct {
	io.Writer
}

func (noopWriteCloser) Close() error { return nil }

func (noopProgressBar) WrapReader(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

func (noopProgressBar) WrapWriter(w io.Writer) io.WriteCloser {
	return noopWriteCloser{Writer: w}
}

func (noopProgressBar) AddBytes(int64) {}
func (noopProgressBar) SetTotal(int64) {}
func (noopProgressBar) Finish(error)   {}

// ciProgressBar is a ProgressBar on top of the line-per-frame ciRenderer,
// used in CI logs without a TTY.
type ciProgressBar struct {
	r *ciRenderer
}

type ciCountingReadCloser struct {
	io.Reader
	closer func() error
}

func (c ciCountingReadCloser) Close() error {
	if c.closer != nil {
		return c.closer()
	}
	return nil
}

type ciCountingWriteCloser struct {
	w   io.Writer
	bar *ciRenderer
}

func (c *ciCountingWriteCloser) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.bar.AddBytes(int64(n))
	}
	return n, err
}

func (c *ciCountingWriteCloser) Close() error { return nil }

func (p *ciProgressBar) WrapReader(r io.Reader) io.ReadCloser {
	cr := &countingReader{r: r, onRead: func(n int64) { p.r.AddBytes(n) }}
	closer, _ := r.(io.Closer)
	return ciCountingReadCloser{Reader: cr, closer: func() error {
		if closer != nil {
			return closer.Close()
		}
		return nil
	}}
}

func (p *ciProgressBar) WrapWriter(w io.Writer) io.WriteCloser {
	return &ciCountingWriteCloser{w: w, bar: p.r}
}

func (p *ciProgressBar) AddBytes(n int64) { p.r.AddBytes(n) }
func (p *ciProgressBar) SetTotal(n int64) { p.r.SetTotal(n) }

// Finish stops the renderer. err is ignored: unlike the mpb bar, the
// line-per-frame CI renderer has no separate visual "aborted" state to
// render, so a failed transfer looks the same as a successful one here.
func (p *ciProgressBar) Finish(error) { p.r.Stop() }

// mpbProgressBar is a ProgressBar on top of *mpb.Progress/*mpb.Bar, used for
// TTY output.
type mpbProgressBar struct {
	p   *mpb.Progress
	bar *mpb.Bar
	// indeterminate records whether the bar was constructed with total <= 0
	// (size unknown ahead of time). mpb only arms triggerComplete for bars
	// built with total > 0, so such a bar's Completed() is always false and
	// Finish must route it through SetTotal(-1, true) instead of Abort.
	indeterminate bool
}

type mpbCountingWriteCloser struct {
	w   io.Writer
	bar *mpb.Bar
}

func (c *mpbCountingWriteCloser) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.bar.IncrBy(n)
	}
	return n, err
}

func (c *mpbCountingWriteCloser) Close() error { return nil }

func (m *mpbProgressBar) WrapReader(r io.Reader) io.ReadCloser {
	return m.bar.ProxyReader(r)
}

func (m *mpbProgressBar) WrapWriter(w io.Writer) io.WriteCloser {
	return &mpbCountingWriteCloser{w: w, bar: m.bar}
}

// AddBytes increments the bar counter by n bytes. n is chunked to
// math.MaxInt32 per call so a single large increment cannot overflow int on
// 32-bit platforms, where int is only 32 bits wide.
func (m *mpbProgressBar) AddBytes(n int64) {
	if n <= 0 {
		return
	}
	for n > math.MaxInt32 {
		m.bar.IncrBy(math.MaxInt32)
		n -= math.MaxInt32
	}
	m.bar.IncrBy(int(n))
}

// SetTotal updates the bar's total, ignoring non-positive values. mpb's own
// SetTotal treats a negative n as "freeze total at the current counter"
// rather than "unknown", which would lock an in-progress bar at a stale
// total (often 0, right after headers arrive and before any bytes are
// read) and make it render as falsely complete. Skipping n<=0 here keeps
// the bar's original indeterminate total instead, matching how the CI
// renderer treats a non-positive total as unknown.
func (m *mpbProgressBar) SetTotal(n int64) {
	if n > 0 {
		m.bar.SetTotal(n, false)
	}
}

func (m *mpbProgressBar) Finish(err error) {
	switch {
	case err != nil:
		m.bar.Abort(false)
	case m.indeterminate:
		// Bars constructed with total <= 0 never have mpb's triggerComplete
		// armed, so Completed() would stay false forever; SetTotal(-1, true)
		// freezes the total at whatever was actually written and completes
		// the bar, letting p.Wait() return.
		m.bar.SetTotal(-1, true)
	case !m.bar.Completed():
		// Bars constructed with total > 0 have triggerComplete set, which
		// makes SetTotal a no-op; aborting (without dropping the bar from
		// output) is the only way to guarantee p.Wait() returns when
		// current never reached total.
		m.bar.Abort(false)
	default:
		m.bar.SetTotal(-1, true)
	}
	m.p.Wait()
}

// NewProgressBar assembles a ProgressBar, choosing between mpb and the CI
// renderer by the same rules as FetchURL: if Stderr is a TTY and CIMode is
// unset, mpb is used; otherwise the line-per-frame CI renderer is used. The
// context stops the background render loop in both mpb and CI modes.
func NewProgressBar(ctx context.Context, opts ProgressOptions) ProgressBar {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.NoProgressBar {
		return noopProgressBar{}
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	refreshRate := opts.RefreshRate
	if refreshRate <= 0 {
		refreshRate = defaultProgressRefresh
	}
	if useCIRenderer(opts.Stderr, opts.CIMode) {
		r := newCIRenderer(opts.Stderr, opts.Total, opts.Reverse)
		r.Start(ctx)
		return &ciProgressBar{r: r}
	}
	style := mpb.BarStyle().Rbound("|")
	if opts.Reverse {
		style = style.Reverse()
	}
	prepend := []decor.Decorator{decor.CountersKibiByte("% .2f / % .2f")}
	if opts.Name != "" {
		prepend = append(
			[]decor.Decorator{decor.Name(opts.Name + " ")}, prepend...,
		)
	}
	p := mpb.NewWithContext(
		ctx,
		mpb.WithOutput(opts.Stderr),
		mpb.WithRefreshRate(refreshRate),
	)
	bar := p.New(
		opts.Total,
		style,
		mpb.PrependDecorators(prepend...),
		mpb.AppendDecorators(
			decor.EwmaSpeed(decor.SizeB1024(0), " % .2f", 60),
			decor.Name(" "),
			decor.EwmaETA(decor.ET_STYLE_GO, 60),
		),
	)
	return &mpbProgressBar{p: p, bar: bar, indeterminate: opts.Total <= 0}
}
