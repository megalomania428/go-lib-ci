// cspell:ignore EWMA vbauerster
package ci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vbauerster/mpb/v8"
)

func ptrBool(b bool) *bool { return &b }

func Test_useCIRenderer(t *testing.T) {
	trueVal, falseVal := true, false
	t.Run("CIMode forced true", func(t *testing.T) {
		if !useCIRenderer(os.Stdout, &trueVal) {
			t.Error("expected true when ciMode=true")
		}
	})
	t.Run("CIMode forced false", func(t *testing.T) {
		if useCIRenderer(os.Stdout, &falseVal) {
			t.Error("expected false when ciMode=false")
		}
	})
	t.Run("non-file stderr is CI", func(t *testing.T) {
		if !useCIRenderer(&bytes.Buffer{}, nil) {
			t.Error("expected true for non-*os.File stderr")
		}
	})
	t.Run("os.File non-tty is CI", func(t *testing.T) {
		// /dev/null is a file but not a terminal.
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Skip("cannot open /dev/null:", err)
		}
		defer f.Close()
		if !useCIRenderer(f, nil) {
			t.Error("expected true for non-tty os.File")
		}
	})
}

func Test_noopWriteCloser_Close(t *testing.T) {
	buf := &bytes.Buffer{}
	n := noopWriteCloser{Writer: buf}
	if _, err := n.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != "payload" {
		t.Errorf("buf = %q, want %q", buf.String(), "payload")
	}
	if err := n.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func Test_noopProgressBar_WrapReader(t *testing.T) {
	n := noopProgressBar{}
	src := strings.NewReader("hello")
	rc := n.WrapReader(src)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("data = %q, want %q", got, "hello")
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func Test_noopProgressBar_WrapReader_PassesThroughReadCloser(t *testing.T) {
	n := noopProgressBar{}
	src := io.NopCloser(strings.NewReader("hello"))
	rc := n.WrapReader(src)
	if rc != src {
		t.Errorf("WrapReader() did not return the original io.ReadCloser")
	}
}

func Test_noopProgressBar_WrapWriter(t *testing.T) {
	n := noopProgressBar{}
	buf := &bytes.Buffer{}
	wc := n.WrapWriter(buf)
	if _, err := wc.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
	if err := wc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func Test_noopProgressBar_AddBytes(_ *testing.T) {
	// AddBytes on the no-op implementation must not panic.
	noopProgressBar{}.AddBytes(1024)
}

func Test_noopProgressBar_SetTotal(_ *testing.T) {
	// SetTotal on the no-op implementation must not panic.
	noopProgressBar{}.SetTotal(2048)
}

func Test_noopProgressBar_Finish(_ *testing.T) {
	// Finish on the no-op implementation must not panic, with or without an
	// error.
	noopProgressBar{}.Finish(nil)
	noopProgressBar{}.Finish(errors.New("boom"))
}

func Test_ciCountingReadCloser_Close(t *testing.T) {
	t.Run("nil closer is a no-op", func(t *testing.T) {
		c := ciCountingReadCloser{Reader: strings.NewReader("x")}
		if err := c.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	t.Run("closer is invoked", func(t *testing.T) {
		called := false
		c := ciCountingReadCloser{
			Reader: strings.NewReader("x"),
			closer: func() error {
				called = true
				return nil
			},
		}
		if err := c.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
		if !called {
			t.Error("expected closer to be invoked")
		}
	})
	t.Run("closer error propagates", func(t *testing.T) {
		wantErr := errors.New("close failed")
		c := ciCountingReadCloser{
			Reader: strings.NewReader("x"),
			closer: func() error { return wantErr },
		}
		if err := c.Close(); !errors.Is(err, wantErr) {
			t.Errorf("Close() error = %v, want %v", err, wantErr)
		}
	})
}

func Test_ciCountingWriteCloser_Write(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newCIRenderer(io.Discard, 100, false)
	c := &ciCountingWriteCloser{w: buf, bar: r}
	n, err := c.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
	if got := r.counter.Load(); got != 5 {
		t.Errorf("counter = %d, want 5", got)
	}
}

func Test_ciCountingWriteCloser_Close(t *testing.T) {
	r := newCIRenderer(io.Discard, 100, false)
	c := &ciCountingWriteCloser{w: io.Discard, bar: r}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func Test_ciProgressBar_WrapReader(t *testing.T) {
	t.Run("non-closer source", func(t *testing.T) {
		r := newCIRenderer(io.Discard, 100, false)
		p := &ciProgressBar{r: r}
		src := strings.NewReader("hello")
		rc := p.WrapReader(src)
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != "hello" {
			t.Errorf("data = %q, want %q", got, "hello")
		}
		if gotCounter := r.counter.Load(); gotCounter != 5 {
			t.Errorf("counter = %d, want 5", gotCounter)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	t.Run("closer source is closed", func(t *testing.T) {
		r := newCIRenderer(io.Discard, 100, false)
		p := &ciProgressBar{r: r}
		src := io.NopCloser(strings.NewReader("hello"))
		rc := p.WrapReader(src)
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
}

func Test_ciProgressBar_WrapWriter(t *testing.T) {
	r := newCIRenderer(io.Discard, 100, false)
	p := &ciProgressBar{r: r}
	buf := &bytes.Buffer{}
	wc := p.WrapWriter(buf)
	if _, err := wc.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
	if gotCounter := r.counter.Load(); gotCounter != 5 {
		t.Errorf("counter = %d, want 5", gotCounter)
	}
	if err := wc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func Test_ciProgressBar_AddBytes(t *testing.T) {
	r := newCIRenderer(io.Discard, 100, false)
	p := &ciProgressBar{r: r}
	p.AddBytes(42)
	if got := r.counter.Load(); got != 42 {
		t.Errorf("counter = %d, want 42", got)
	}
}

func Test_ciProgressBar_SetTotal(t *testing.T) {
	r := newCIRenderer(io.Discard, 100, false)
	p := &ciProgressBar{r: r}
	p.SetTotal(555)
	if got := r.total.Load(); got != 555 {
		t.Errorf("total = %d, want 555", got)
	}
}

func Test_ciProgressBar_Finish(t *testing.T) {
	out := &bytes.Buffer{}
	r := newCIRenderer(out, 10, false)
	r.Start(context.Background())
	r.AddBytes(5)
	r.frame()
	r.AddBytes(5)
	p := &ciProgressBar{r: r}
	p.Finish(nil)
	if out.String() == "" {
		t.Error("expected Finish to flush a final line")
	}
}

func Test_mpbCountingWriteCloser_Write(t *testing.T) {
	// Total is left unknown (0), so the bar is indeterminate and Finish
	// routes it through SetTotal(-1, true) to set the final total to
	// whatever was actually written, avoiding a deadlock in
	// mpb.Progress.Wait() when current never reaches a preset total.
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
	})
	m, ok := p.(*mpbProgressBar)
	if !ok {
		t.Fatal("expected *mpbProgressBar")
	}
	buf := &bytes.Buffer{}
	c := &mpbCountingWriteCloser{w: buf, bar: m}
	n, err := c.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
	p.Finish(nil)
}

func Test_mpbCountingWriteCloser_Close(t *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
	})
	m, ok := p.(*mpbProgressBar)
	if !ok {
		t.Fatal("expected *mpbProgressBar")
	}
	c := &mpbCountingWriteCloser{w: io.Discard, bar: m}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	p.Finish(nil)
}

func Test_mpbProgressBar_WrapReader(t *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  5,
	})
	src := strings.NewReader("hello")
	rc := p.WrapReader(src)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("data = %q, want %q", got, "hello")
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	p.Finish(nil)
}

func Test_mpbProgressBar_WrapWriter(t *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  5,
	})
	buf := &bytes.Buffer{}
	wc := p.WrapWriter(buf)
	if _, err := wc.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello")
	}
	if err := wc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	p.Finish(nil)
}

func Test_mpbProgressBar_AddBytes(_ *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  100,
	})
	// AddBytes must not panic on the mpb-backed implementation. It reaches
	// the preset total so the bar's own completion trigger fires and Finish
	// does not have to force it via SetTotal(-1, true).
	p.AddBytes(100)
	p.Finish(nil)
}

// Test_mpbProgressBar_AddBytes_NonPositive covers the early-return branch:
// a zero or negative increment must not touch the underlying mpb.Bar.
func Test_mpbProgressBar_AddBytes_NonPositive(_ *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  100,
	})
	p.AddBytes(0)
	p.AddBytes(-1)
	p.Finish(errors.New("unused"))
}

// Test_mpbProgressBar_AddBytes_LargeIncrement covers an increment larger
// than math.MaxInt32, which reaches mpb as a single int64 EWMA increment.
func Test_mpbProgressBar_AddBytes_LargeIncrement(_ *testing.T) {
	const total = int64(math.MaxInt32) + 10
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  total,
	})
	p.AddBytes(total)
	p.Finish(nil)
}

func Test_mpbProgressBar_SetTotal(_ *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  0,
	})
	// SetTotal must not panic on the mpb-backed implementation.
	p.SetTotal(200)
	p.Finish(nil)
}

// Test_mpbProgressBar_SetTotal_NonPositive covers the guard branch: a zero
// or negative total must not reach the underlying mpb.Bar, since mpb treats
// a negative total as "freeze at current counter" rather than "unknown",
// which would falsely mark an in-progress bar as complete.
func Test_mpbProgressBar_SetTotal_NonPositive(_ *testing.T) {
	p := NewProgressBar(context.Background(), ProgressOptions{
		Stderr: io.Discard,
		CIMode: ptrBool(false),
		Total:  -1,
	})
	p.SetTotal(0)
	p.SetTotal(-1)
	p.Finish(errors.New("unused"))
}

func Test_mpbProgressBar_Finish(t *testing.T) {
	t.Run("success does not panic", func(_ *testing.T) {
		p := NewProgressBar(context.Background(), ProgressOptions{
			Stderr: io.Discard,
			CIMode: ptrBool(false),
			Total:  10,
		})
		p.AddBytes(10)
		p.Finish(nil)
	})
	t.Run("error aborts without panic", func(_ *testing.T) {
		p := NewProgressBar(context.Background(), ProgressOptions{
			Stderr: io.Discard,
			CIMode: ptrBool(false),
			Total:  10,
		})
		p.Finish(errors.New("boom"))
	})
	// A bar constructed with total > 0 has mpb's triggerComplete set, which
	// makes SetTotal(-1, true) a no-op; Finish must abort instead of hanging
	// in p.Wait() forever when fewer bytes were written than Total.
	t.Run("success with unreached total does not deadlock", func(t *testing.T) {
		p := NewProgressBar(context.Background(), ProgressOptions{
			Stderr: io.Discard,
			CIMode: ptrBool(false),
			Total:  10000,
		})
		p.AddBytes(5000)
		done := make(chan struct{})
		go func() {
			p.Finish(nil)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Finish did not return: p.Wait() deadlocked")
		}
	})
}

// TestNewProgressBar_NilContext verifies that a nil context is replaced with
// Background so the CI renderer loop does not panic on ctx.Done().
func TestNewProgressBar_NilContext(_ *testing.T) {
	var ctx context.Context
	bar := NewProgressBar(ctx, ProgressOptions{
		Total:  1,
		Stderr: io.Discard,
		CIMode: ptrBool(true),
	})
	bar.AddBytes(1)
	bar.Finish(nil)
}

// TestNewProgressBar_NoOp verifies that NoProgressBar produces a bar that
// forwards bytes unchanged and never writes to Stderr.
func TestNewProgressBar_NoOp(t *testing.T) {
	stderr := &bytes.Buffer{}
	bar := NewProgressBar(context.Background(), ProgressOptions{
		NoProgressBar: true,
		Stderr:        stderr,
	})
	rc := bar.WrapReader(strings.NewReader("upload payload"))
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "upload payload" {
		t.Errorf("data = %q, want %q", got, "upload payload")
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	bar.AddBytes(10)
	bar.SetTotal(100)
	bar.Finish(nil)
	if stderr.Len() != 0 {
		t.Errorf("Stderr = %q, want empty", stderr.String())
	}
}

// TestNewProgressBar_CIRenderer_ForwardMatchesReverseInBytes checks that the
// same input in forward and reverse CI mode produces the same number of
// printed lines, and that the fill direction differs as documented.
func TestNewProgressBar_CIRenderer_ForwardMatchesReverseInBytes(t *testing.T) {
	const total = int64(4096)

	run := func(reverse bool) string {
		stderr := &bytes.Buffer{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		bar := NewProgressBar(ctx, ProgressOptions{
			CIMode:  ptrBool(true),
			Total:   total,
			Stderr:  stderr,
			Reverse: reverse,
		})
		// Add bytes below the total in two steps, calling frame()
		// deterministically between them, so a mid-transfer frame is
		// captured (cur < total) before Finish reports the final 100% line.
		cp := bar.(*ciProgressBar)
		bar.AddBytes(total / 4)
		cp.r.frame()
		bar.AddBytes(total / 4)
		cp.r.frame()
		bar.Finish(nil)
		return stderr.String()
	}

	forward := run(false)
	reverse := run(true)

	forwardLines := strings.Count(forward, "\n")
	reverseLines := strings.Count(reverse, "\n")
	if forwardLines != reverseLines {
		t.Errorf(
			"line count mismatch: forward=%d reverse=%d", forwardLines, reverseLines,
		)
	}
	if !strings.Contains(forward, "[===") || !strings.Contains(forward, ">-") {
		t.Errorf("forward output missing expected substrings: %q", forward)
	}
	if !strings.Contains(reverse, "-<") || !strings.Contains(reverse, "===]") {
		t.Errorf("reverse output missing expected substrings: %q", reverse)
	}
}

// TestNewProgressBar_WrapWriter_CountsBytes writes 10000 bytes through
// WrapWriter and checks that the final CI line reports 100%.
func TestNewProgressBar_WrapWriter_CountsBytes(t *testing.T) {
	stderr := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bar := NewProgressBar(ctx, ProgressOptions{
		CIMode: ptrBool(true),
		Total:  10000,
		Stderr: stderr,
	})
	cp := bar.(*ciProgressBar)
	wc := bar.WrapWriter(io.Discard)
	payload := bytes.Repeat([]byte("y"), 10000)
	if _, err := wc.Write(payload[:5000]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cp.r.frame()
	if _, err := wc.Write(payload[5000:]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cp.r.frame()
	bar.Finish(nil)
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		t.Fatal("expected at least one printed line")
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "100%") {
		t.Errorf("last line = %q, want it to contain 100%%", last)
	}
}

// TestNewProgressBar_MpbMode_ReverseCopiesPayload checks that the mpb
// renderer with Reverse: true does not panic and copies every byte through.
func TestNewProgressBar_MpbMode_ReverseCopiesPayload(t *testing.T) {
	stderr := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bar := NewProgressBar(ctx, ProgressOptions{
		Name:    "upload.bin",
		CIMode:  ptrBool(false),
		Total:   1000,
		Stderr:  stderr,
		Reverse: true,
	})
	payload := strings.Repeat("z", 1000)
	dst := &bytes.Buffer{}
	rc := bar.WrapReader(strings.NewReader(payload))
	if _, err := io.Copy(dst, rc); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	_ = rc.Close()
	bar.Finish(nil)
	if dst.String() != payload {
		t.Errorf("copied payload mismatch: got %d bytes, want %d",
			dst.Len(), len(payload))
	}
}

// TestNewProgressBar_DefaultStderr checks that an omitted Stderr falls back
// to os.Stderr without panicking.
func TestNewProgressBar_DefaultStderr(_ *testing.T) {
	bar := NewProgressBar(context.Background(), ProgressOptions{
		CIMode: ptrBool(true),
	})
	bar.Finish(nil)
}

// TestNewProgressBar_SetTotalAfterStart exercises the exact path downloadOnce
// takes: the bar starts indeterminate (Total: -1) so the wave animation
// covers connect/TLS, then SetTotal is called once the response arrives.
func TestNewProgressBar_SetTotalAfterStart(t *testing.T) {
	var buf bytes.Buffer
	bar := NewProgressBar(context.Background(), ProgressOptions{
		Total:  -1,
		Stderr: &buf,
		CIMode: ptrBool(true),
	})
	bar.SetTotal(10)
	src := bytes.NewReader(bytes.Repeat([]byte("x"), 10))
	wrapped := bar.WrapReader(src)
	if _, err := io.Copy(io.Discard, wrapped); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	_ = wrapped.Close()
	bar.Finish(nil)
	if strings.Contains(buf.String(), "100%") {
		t.Fatalf("did not expect 100%% for a tiny transfer, got %q", buf.String())
	}
}

// TestCIRenderer_SmallTransferSuppressesFinal100 checks that when the transfer
// is small enough that no mid-progress frame is ever printed, Stop does not
// emit a lone 100% line. This covers the "previous output was empty" branch
// of printPVIfChanged/Stop.
func TestCIRenderer_SmallTransferSuppressesFinal100(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bar := NewProgressBar(ctx, ProgressOptions{
		CIMode:      ptrBool(true),
		Total:       10,
		Stderr:      &buf,
		RefreshRate: time.Hour,
	})
	bar.AddBytes(10)
	bar.Finish(nil)
	if strings.Contains(buf.String(), "100%") {
		t.Fatalf("did not expect 100%% for tiny transfer, got %q", buf.String())
	}
}

// TestCIRenderer_FinalNotDuplicatedAfter100 checks that when the tick loop
// already printed a 100% line, Stop does not print a second one. This covers
// the "lastPct >= 100" branch of Stop.
func TestCIRenderer_FinalNotDuplicatedAfter100(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bar := NewProgressBar(ctx, ProgressOptions{
		CIMode: ptrBool(true),
		Total:  4096,
		Stderr: &buf,
	})
	cp := bar.(*ciProgressBar)
	bar.AddBytes(1024)
	cp.r.frame()
	bar.AddBytes(3072)
	cp.r.frame()
	bar.Finish(nil)
	if strings.Count(buf.String(), "100%") != 1 {
		t.Fatalf("expected exactly one 100%% line, got %q", buf.String())
	}
}

func Test_ciRenderer_doubleStart(_ *testing.T) {
	r := newCIRenderer(io.Discard, 100, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Start(ctx) // second call is a no-op
	cancel()
	r.Stop()
}

func Test_ciRenderer_SetTotal(t *testing.T) {
	r := newCIRenderer(io.Discard, -1, false)
	r.SetTotal(1024)
	if got := r.total.Load(); got != 1024 {
		t.Errorf("SetTotal: got %d, want 1024", got)
	}
}

func Test_ciRenderer_AddBytes(t *testing.T) {
	r := newCIRenderer(io.Discard, 0, false)
	r.AddBytes(100)
	r.AddBytes(0) // zero must not change counter
	if got := r.counter.Load(); got != 100 {
		t.Errorf("AddBytes: got %d, want 100", got)
	}
}

func Test_ciRenderer_loopTickerFires(t *testing.T) {
	// syncWriter counts writes atomically so we can check from the test
	// goroutine without a data race on bytes.Buffer.
	var written atomic.Int64
	w := writerFunc(func(p []byte) (int, error) {
		written.Add(int64(len(p)))
		return len(p), nil
	})
	r := newCIRenderer(w, 0, false)
	r.mu.Lock()
	r.startedAt = time.Now()
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	// Wait until at least one ticker frame has been printed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if written.Load() > 0 {
			break
		}
		time.Sleep(ciRefreshPeriod)
	}
	cancel()
	r.Stop()
	if written.Load() == 0 {
		t.Error("loop must call frame() on ticker tick")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func Test_printPVIfChanged_suppressedWhenUnchanged(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newCIRenderer(buf, 1000, false)
	r.mu.Lock()
	r.startedAt = time.Now()
	now := time.Now()
	r.printPVIfChanged(500, 1000, now)
	first := buf.Len()
	// Same bar position, same pct, within 1s window → must be suppressed.
	r.printPVIfChanged(500, 1000, now.Add(100*time.Millisecond))
	r.mu.Unlock()
	if buf.Len() != first {
		t.Error("expected suppression when nothing changed within 1s")
	}
}

func Test_renderWaveLine_barBoundaries(t *testing.T) {
	r := newCIRenderer(io.Discard, 0, false)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Drive barPos to the right boundary and back.
	for i := 0; i < ciBarWidth*2; i++ {
		r.renderWaveLine(ciBarWidth, true)
		// barPos is clamped to [0, ciBarWidth-ciBarMarkerLen-1].
		if r.barPos < 0 || r.barPos > ciBarWidth-ciBarMarkerLen-1 {
			t.Fatalf("barPos out of range: %d at iteration %d", r.barPos, i)
		}
	}
}

func Test_humanBytes(t *testing.T) {
	tests := []struct {
		n     int64
		short bool
		want  string
	}{
		{-1, true, "0B"},
		{0, true, "0B"},
		{1023, true, "1023B"},
		{1024, true, "1.0K"},
		{1536, true, "1.5K"},
		{100 * 1024, true, "100K"},
		{1024 * 1024, true, "1.0M"},
		{1024 * 1024 * 1024, true, "1.0G"},
		{1024 * 1024 * 1024 * 1024, true, "1.0T"},
		{512 * 1024 * 1024 * 1024 * 1024, true, "512T"},
		// P-range: force into petabyte to hit last suffix.
		{1024 * 1024 * 1024 * 1024 * 1024, true, "1.0P"},
		{-1, false, "0 B"},
		{0, false, "0 B"},
		{1023, false, "1023 B"},
		{1024, false, "1.0 KiB"},
		{1024 * 1024, false, "1.0 MiB"},
		{1024 * 1024 * 1024, false, "1.0 GiB"},
	}
	for _, tt := range tests {
		got := humanBytes(tt.n, tt.short)
		if got != tt.want {
			t.Errorf("humanBytes(%d, short=%v) = %q, want %q", tt.n, tt.short, got, tt.want)
		}
	}
}

func Test_formatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0:00:00"},
		{0, "0:00:00"},
		{time.Second, "0:00:01"},
		{66 * time.Second, "0:01:06"},
		{358 * time.Second, "0:05:58"},
		{3661 * time.Second, "1:01:01"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func Test_ciCountingReader_Read(t *testing.T) {
	r := newCIRenderer(io.Discard, 0, false)
	t.Run("forwards bytes and counts", func(t *testing.T) {
		cr := &countingReader{r: strings.NewReader("hello"), onRead: r.AddBytes}
		buf := make([]byte, 5)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("read %d bytes, want 5", n)
		}
		if r.counter.Load() != 5 {
			t.Errorf("counter = %d, want 5", r.counter.Load())
		}
	})
	t.Run("zero-read does not increment", func(t *testing.T) {
		r2 := newCIRenderer(io.Discard, 0, false)
		cr := &countingReader{r: strings.NewReader("x"), onRead: r2.AddBytes}
		buf := make([]byte, 0)
		_, _ = cr.Read(buf)
		if r2.counter.Load() != 0 {
			t.Errorf("counter must stay 0 on zero-length read")
		}
	})
	t.Run("error propagates", func(t *testing.T) {
		r3 := newCIRenderer(io.Discard, 0, false)
		cr := &countingReader{r: &errReader{err: errors.New("ci-boom")}, onRead: r3.AddBytes}
		_, err := cr.Read(make([]byte, 4))
		if err == nil || !strings.Contains(err.Error(), "ci-boom") {
			t.Errorf("expected ci-boom error, got %v", err)
		}
	})
}

func Test_newCIRenderer(t *testing.T) {
	type args struct {
		total int64
	}
	tests := []struct {
		name      string
		args      args
		wantTotal int64
	}{
		{name: "negative total stored as-is", args: args{total: -1}, wantTotal: -1},
		{name: "zero total stored", args: args{total: 0}, wantTotal: 0},
		{name: "positive total stored", args: args{total: 1024}, wantTotal: 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			got := newCIRenderer(out, tt.args.total, false)
			if got == nil {
				t.Fatal("newCIRenderer returned nil")
			}
			if got.total.Load() != tt.wantTotal {
				t.Errorf("total = %d, want %d", got.total.Load(), tt.wantTotal)
			}
			if got.out != io.Writer(out) {
				t.Error("out writer not wired")
			}
			if got.stop == nil || got.done == nil {
				t.Error("stop/done channels must be allocated")
			}
			if got.barMove != 1 || got.tick != 150 {
				t.Errorf("default state mismatch: barMove=%d tick=%d",
					got.barMove, got.tick)
			}
			if out.String() != "" {
				t.Errorf("ctor must not write, got %q", out.String())
			}
		})
	}
}

func Test_ciRenderer_Start(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "start spawns loop that exits on context cancel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 100, false)
			ctx, cancel := context.WithCancel(context.Background())
			r.Start(ctx)
			if !r.started {
				t.Error("Start must set started flag")
			}
			if r.startedAt.IsZero() {
				t.Error("Start must record startedAt")
			}
			cancel()
			select {
			case <-r.done:
			case <-time.After(time.Second):
				t.Fatal("loop did not exit after context cancel")
			}
		})
	}
}

func Test_ciRenderer_Stop(t *testing.T) {
	tests := []struct {
		name         string
		startFirst   bool
		total        int64
		counter      int64 // value stored in r.counter before Stop; -1 means leave at zero
		lastBar      int
		lastPct      float64 // percentage of the last printed frame, if any
		hadPrinted   bool    // whether the renderer already printed a frame
		wantContains string
		wantOmit     string
		elements     progressElements // 0 keeps the full line
		wantEmpty    bool
	}{
		{name: "stop before start does not panic",
			startFirst: false, total: 0, counter: -1, lastBar: 0},
		{name: "stop after full download prints final 100 percent line",
			startFirst: true, total: 1024, counter: 1024, lastBar: 10,
			lastPct: 50, hadPrinted: true, wantContains: "100%"},
		{name: "stop after partial download does not print 100 percent",
			startFirst: true, total: 1024, counter: 512, lastBar: 10,
			lastPct: 50, hadPrinted: true, wantOmit: "100%"},
		{name: "stop with zero total prints no final line",
			startFirst: true, total: 0, counter: -1, lastBar: 10,
			lastPct: 50, hadPrinted: true, wantOmit: "100%"},
		{name: "stop without any prior print prints no final line",
			startFirst: true, total: 1024, counter: 1024, lastBar: 10,
			wantOmit: "100%"},
		{name: "stop after prior 100 percent print does not duplicate it",
			startFirst: true, total: 1024, counter: 1024, lastBar: 10,
			lastPct: 100, hadPrinted: true, wantOmit: "100%"},
		{name: "stop prints nothing when the final line is empty",
			startFirst: true, total: 1024, counter: 1024, lastBar: 10,
			lastPct: 50, hadPrinted: true,
			elements: elementsOf(ProgressElementETA), wantEmpty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, tt.total, false)
			if tt.startFirst {
				ctx, cancel := context.WithCancel(context.Background())
				r.Start(ctx)
				defer cancel()
				r.mu.Lock()
				r.startedAt = time.Now()
				r.lastBar = tt.lastBar
				r.lastPct = tt.lastPct
				if tt.elements != 0 {
					r.elements = tt.elements
				}
				if tt.hadPrinted {
					r.lastPrint = time.Now()
				}
				r.mu.Unlock()
			}
			if tt.counter >= 0 {
				r.counter.Store(tt.counter)
			}
			r.Stop()
			got := buf.String()
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("Stop output = %q, want substring %q", got, tt.wantContains)
			}
			if tt.wantOmit != "" && strings.Contains(got, tt.wantOmit) {
				t.Errorf("Stop output = %q, must not contain %q", got, tt.wantOmit)
			}
			if tt.wantEmpty && got != "" {
				t.Errorf("Stop output = %q, want empty", got)
			}
		})
	}
}

// Test_ciRenderer_doubleStop exercises the stopped-flag branch in Stop:
// after the first call closes r.stop, the second call must see stopped and
// skip close (which would panic on an already-closed channel).
func Test_ciRenderer_doubleStop(_ *testing.T) {
	r := newCIRenderer(io.Discard, 0, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Stop()
	r.Stop() // second call must not panic on an already-closed stop channel
}

func Test_ciRenderer_loop(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(r *ciRenderer, cancel context.CancelFunc)
	}{
		{
			name: "loop exits when context canceled",
			trigger: func(_ *ciRenderer, cancel context.CancelFunc) {
				time.AfterFunc(20*time.Millisecond, cancel)
			},
		},
		{
			name: "loop exits when stop channel closed",
			trigger: func(r *ciRenderer, _ context.CancelFunc) {
				time.AfterFunc(20*time.Millisecond, func() { close(r.stop) })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.trigger(r, cancel)
			r.loop(ctx)
			select {
			case <-r.done:
			default:
				t.Fatal("loop must close done channel on exit")
			}
		})
	}
}

func Test_ciRenderer_frame(t *testing.T) {
	tests := []struct {
		name         string
		counter      int64
		total        int64
		wantNonEmpty bool
		wantContains string
	}{
		{name: "zero counter prints wave line", counter: 0, total: 1024, wantNonEmpty: true},
		{name: "bytes received with unknown total prints wave plus bytes",
			counter: 512, total: -1, wantContains: "B"},
		{name: "bytes received with known total prints pv line",
			counter: 512, total: 1024, wantContains: "%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, tt.total, false)
			r.startedAt = time.Now()
			r.counter.Store(tt.counter)
			r.frame()
			got := buf.String()
			if tt.wantNonEmpty && got == "" {
				t.Errorf("frame output empty, want a wave line")
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("frame output = %q, want substring %q",
					got, tt.wantContains)
			}
		})
	}
}

func Test_ciRenderer_updateSpeed(t *testing.T) {
	type fields struct {
		speedEWMA   float64
		speedLastAt time.Time
		speedLastN  int64
	}
	type args struct {
		cur int64
		now time.Time
	}
	base := time.Now()
	tests := []struct {
		name             string
		fields           fields
		args             args
		wantEWMAExact    float64
		wantEWMAPositive bool
		wantEWMAAtMost   float64
		wantLastAtSet    bool
	}{
		{
			name: "first call initializes timestamp without computing EWMA",
			fields: fields{speedEWMA: 0,
				speedLastAt: time.Time{}, speedLastN: 0},
			args:          args{cur: 0, now: base},
			wantEWMAExact: 0, wantLastAtSet: true,
		},
		{
			name:          "zero dt leaves EWMA untouched",
			fields:        fields{speedEWMA: 0, speedLastAt: base, speedLastN: 0},
			args:          args{cur: 1000, now: base},
			wantEWMAExact: 0,
		},
		{
			name:             "positive dt computes positive EWMA",
			fields:           fields{speedEWMA: 0, speedLastAt: base, speedLastN: 0},
			args:             args{cur: 1000, now: base.Add(time.Second)},
			wantEWMAPositive: true,
		},
		{
			name:           "negative instantaneous rate must not raise EWMA",
			fields:         fields{speedEWMA: 500, speedLastAt: base, speedLastN: 1000},
			args:           args{cur: 0, now: base.Add(time.Second)},
			wantEWMAAtMost: 500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0, false)
			r.speedEWMA = tt.fields.speedEWMA
			r.speedLastAt = tt.fields.speedLastAt
			r.speedLastN = tt.fields.speedLastN
			r.updateSpeed(tt.args.cur, tt.args.now)
			if tt.wantLastAtSet && r.speedLastAt.IsZero() {
				t.Error("updateSpeed must set speedLastAt on first call")
			}
			if tt.wantEWMAPositive && r.speedEWMA <= 0 {
				t.Errorf("expected positive EWMA, got %f", r.speedEWMA)
			}
			if tt.wantEWMAAtMost > 0 && r.speedEWMA > tt.wantEWMAAtMost {
				t.Errorf("EWMA must not exceed %f, got %f",
					tt.wantEWMAAtMost, r.speedEWMA)
			}
			if !tt.wantEWMAPositive && tt.wantEWMAAtMost == 0 &&
				r.speedEWMA != tt.wantEWMAExact {
				t.Errorf("EWMA = %f, want %f", r.speedEWMA, tt.wantEWMAExact)
			}
		})
	}
}

func Test_ciRenderer_printPVIfChanged(t *testing.T) {
	type fields struct {
		lastBar   int
		lastPrint time.Time
		lastPct   float64
		elements  progressElements // 0 keeps the full line
	}
	type args struct {
		cur   int64
		total int64
		now   time.Time
	}
	now := time.Now()
	tests := []struct {
		name        string
		fields      fields
		args        args
		wantPrinted bool
	}{
		{
			name:        "first call always prints",
			fields:      fields{lastBar: 0, lastPrint: time.Time{}},
			args:        args{cur: 500, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name:        "bar advance forces print within window",
			fields:      fields{lastBar: 0, lastPrint: now},
			args:        args{cur: 900, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name:        "stale entry older than one second prints",
			fields:      fields{lastBar: -1, lastPrint: now.Add(-2 * time.Second)},
			args:        args{cur: 500, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name: "duplicate 100 percent after prior 100 percent print is suppressed",
			fields: fields{
				lastBar: -1, lastPrint: now.Add(-2 * time.Second), lastPct: 100,
			},
			args:        args{cur: 1000, total: 1000, now: now},
			wantPrinted: false,
		},
		{
			name: "without bar the same integer percent is suppressed",
			fields: fields{
				lastBar: 50, lastPrint: now, elements: elementsOf(ProgressElementPercent),
			},
			args:        args{cur: 505, total: 1000, now: now},
			wantPrinted: false,
		},
		{
			name: "without bar a new integer percent prints",
			fields: fields{
				lastBar: 50, lastPrint: now, elements: elementsOf(ProgressElementPercent),
			},
			args:        args{cur: 510, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name: "empty line is not printed",
			fields: fields{
				lastBar: -1, lastPrint: now.Add(-2 * time.Second), lastPct: 50,
				elements: elementsOf(ProgressElementETA),
			},
			args:        args{cur: 1000, total: 1000, now: now},
			wantPrinted: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, tt.args.total, false)
			r.startedAt = now
			r.lastBar = tt.fields.lastBar
			r.lastPrint = tt.fields.lastPrint
			r.lastPct = tt.fields.lastPct
			if tt.fields.elements != 0 {
				r.elements = tt.fields.elements
			}
			before := buf.Len()
			r.printPVIfChanged(tt.args.cur, tt.args.total, tt.args.now)
			printed := buf.Len() > before
			if printed != tt.wantPrinted {
				t.Errorf("printPVIfChanged printed=%v, want %v (buf=%q)",
					printed, tt.wantPrinted, buf.String())
			}
		})
	}
}

func Test_ciRenderer_renderFinalLine(t *testing.T) {
	type args struct {
		cur   int64
		total int64
	}
	tests := []struct {
		name         string
		args         args
		wantContains string
	}{
		{name: "complete renders 100 percent",
			args: args{cur: 1024, total: 1024}, wantContains: "100%"},
		{name: "partial renders percent value",
			args: args{cur: 256, total: 1024}, wantContains: "%"},
		{name: "overflow clamps to 100 percent",
			args: args{cur: 2048, total: 1024}, wantContains: "100%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, tt.args.total, false)
			r.startedAt = time.Now()
			got := r.renderFinalLine(tt.args.cur, tt.args.total)
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("ciRenderer.renderFinalLine() = %q, want substring %q",
					got, tt.wantContains)
			}
		})
	}
}

func Test_ciRenderer_pvLine(t *testing.T) {
	type fields struct {
		speedEWMA float64
		elements  progressElements // 0 keeps the full line
		reverse   bool
		elapsed   time.Duration
	}
	type args struct {
		cur   int64
		total int64
	}
	// The examples transfer 109 of 112 MiB at 78.7 MiB/s, one second in.
	example := func(reverse bool, elems ...ProgressElement) fields {
		return fields{
			speedEWMA: 82523750, elements: elementsOf(elems...), reverse: reverse,
			elapsed: time.Second,
		}
	}
	exampleArgs := args{cur: 109 << 20, total: 112 << 20}
	all := []ProgressElement{
		ProgressElementBytes, ProgressElementElapsed, ProgressElementSpeed,
		ProgressElementBar, ProgressElementPercent, ProgressElementETA,
	}
	tests := []struct {
		name           string
		fields         fields
		args           args
		wantFilledGt0  bool
		wantPct        float64
		wantContains   string
		wantOmitString string
		wantLine       string // exact line, checked when not empty
		wantStep       int    // throttling step, checked with wantLine
	}{
		{
			name:          "mid transfer shows ETA",
			fields:        fields{speedEWMA: 0},
			args:          args{cur: 512, total: 1024},
			wantFilledGt0: true, wantPct: 50, wantContains: "ETA",
		},
		{
			name:          "complete hides ETA at 100 percent",
			fields:        fields{speedEWMA: 0},
			args:          args{cur: 1024, total: 1024},
			wantFilledGt0: false, wantPct: 100, wantOmitString: "ETA",
		},
		{
			name:          "overflow clamps to 100 percent",
			fields:        fields{speedEWMA: 0},
			args:          args{cur: 2048, total: 1024},
			wantFilledGt0: false, wantPct: 100, wantContains: "100%",
		},
		{
			name:          "positive EWMA keeps ETA",
			fields:        fields{speedEWMA: 1024},
			args:          args{cur: 256, total: 1024},
			wantFilledGt0: true, wantPct: 25, wantContains: "ETA",
		},
		{
			name: "example full line reversed", fields: example(true, all...),
			args: exampleArgs, wantPct: 97, wantStep: 31,
			wantLine: "109M/112M 0:00:01 [78.7M/s] [<===============================]" +
				"  97% ETA 0:00:00",
		},
		{
			name: "example full line forward", fields: example(false, all...),
			args: exampleArgs, wantPct: 97, wantStep: 31,
			wantLine: "109M/112M 0:00:01 [78.7M/s] [===============================>]" +
				"  97% ETA 0:00:00",
		},
		{
			name: "example without bar",
			fields: example(true, ProgressElementETA, ProgressElementPercent,
				ProgressElementSpeed, ProgressElementElapsed, ProgressElementBytes),
			args: exampleArgs, wantPct: 97, wantStep: 97,
			wantLine: "109M/112M 0:00:01 [78.7M/s]  97% ETA 0:00:00",
		},
		{
			name: "example without bytes and bar",
			fields: example(true, ProgressElementElapsed, ProgressElementSpeed,
				ProgressElementPercent, ProgressElementETA),
			args: exampleArgs, wantPct: 97, wantStep: 97,
			wantLine: "0:00:01 [78.7M/s]  97% ETA 0:00:00",
		},
		{
			name: "example elapsed speed and ETA",
			fields: example(true, ProgressElementElapsed, ProgressElementSpeed,
				ProgressElementETA),
			args: exampleArgs, wantPct: 97, wantStep: 97,
			wantLine: "0:00:01 [78.7M/s] ETA 0:00:00",
		},
		{
			name: "example elapsed percent and ETA",
			fields: example(true, ProgressElementElapsed, ProgressElementPercent,
				ProgressElementETA),
			args: exampleArgs, wantPct: 97, wantStep: 97,
			wantLine: "0:00:01  97% ETA 0:00:00",
		},
		{
			name:   "example bytes and ETA",
			fields: example(true, ProgressElementETA, ProgressElementBytes),
			args:   exampleArgs, wantPct: 97, wantStep: 97,
			wantLine: "109M/112M ETA 0:00:00",
		},
		{
			name: "only bytes", fields: example(false, ProgressElementBytes),
			args: exampleArgs, wantPct: 97, wantStep: 97, wantLine: "109M/112M",
		},
		{
			name: "only elapsed", fields: example(false, ProgressElementElapsed),
			args: exampleArgs, wantPct: 97, wantStep: 97, wantLine: "0:00:01",
		},
		{
			name: "only speed", fields: example(false, ProgressElementSpeed),
			args: exampleArgs, wantPct: 97, wantStep: 97, wantLine: "[78.7M/s]",
		},
		{
			name: "only bar", fields: example(false, ProgressElementBar),
			args: exampleArgs, wantPct: 97, wantStep: 74,
			wantLine: "[" + strings.Repeat("=", 74) + ">--]",
		},
		{
			name: "only percent", fields: example(false, ProgressElementPercent),
			args: exampleArgs, wantPct: 97, wantStep: 97, wantLine: " 97%",
		},
		{
			name: "only ETA", fields: example(false, ProgressElementETA),
			args: exampleArgs, wantPct: 97, wantStep: 97, wantLine: "ETA 0:00:00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, tt.args.total, tt.fields.reverse)
			r.speedEWMA = tt.fields.speedEWMA
			if tt.fields.elements != 0 {
				r.elements = tt.fields.elements
			}
			start := time.Now()
			r.startedAt = start
			got, filled, pct := r.pvLine(tt.args.cur, tt.args.total, start.Add(tt.fields.elapsed))
			if len(got) == 0 {
				t.Error("pvLine must produce a non-empty line")
			}
			if tt.wantFilledGt0 && filled <= 0 {
				t.Errorf("pvLine filled = %d, want > 0", filled)
			}
			if pct < tt.wantPct-1 || pct > tt.wantPct+1 {
				t.Errorf("pvLine pct = %.1f, want ≈ %.0f", pct, tt.wantPct)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("pvLine = %q, want substring %q", got, tt.wantContains)
			}
			if tt.wantOmitString != "" && strings.Contains(got, tt.wantOmitString) {
				t.Errorf("pvLine = %q, must not contain %q", got, tt.wantOmitString)
			}
			if tt.wantLine != "" && (got != tt.wantLine || filled != tt.wantStep) {
				t.Errorf("pvLine = %q, step %d, want %q, step %d",
					got, filled, tt.wantLine, tt.wantStep)
			}
		})
	}
}

func Test_ciRenderer_renderWaveLine(t *testing.T) {
	type args struct {
		w       int
		advance bool
	}
	tests := []struct {
		name             string
		barPos           int
		args             args
		wantLen          int
		wantStateChanges bool
		wantBarPos       int // marker start after the call
	}{
		{name: "advance mutates tick and barPos", barPos: 0,
			args: args{w: ciBarWidth, advance: true}, wantLen: ciBarWidth,
			wantStateChanges: true, wantBarPos: 1},
		{name: "no advance leaves state intact", barPos: 0,
			args: args{w: ciBarWidth, advance: false}, wantLen: ciBarWidth,
			wantStateChanges: false, wantBarPos: 0},
		{name: "barPos at right edge does not panic", barPos: ciBarWidth - ciBarMarkerLen - 1,
			args: args{w: ciBarWidth, advance: false}, wantLen: ciBarWidth,
			wantStateChanges: false, wantBarPos: ciBarWidth - ciBarMarkerLen - 1},
		{name: "narrower line pulls the marker inside",
			barPos: ciBarWidth - ciBarMarkerLen - 1, args: args{w: 30, advance: false},
			wantLen: 30, wantStateChanges: true, wantBarPos: 30 - ciBarMarkerLen - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0, false)
			r.barPos = tt.barPos
			tickBefore, posBefore := r.tick, r.barPos
			got := r.renderWaveLine(tt.args.w, tt.args.advance)
			if len(got) != tt.wantLen {
				t.Errorf("ciRenderer.renderWaveLine() len = %d, want %d",
					len(got), tt.wantLen)
			}
			if r.barPos != tt.wantBarPos {
				t.Errorf("barPos = %d, want %d", r.barPos, tt.wantBarPos)
			}
			changed := r.tick != tickBefore || r.barPos != posBefore
			if changed != tt.wantStateChanges {
				t.Errorf("state changed=%v, want %v (tick %d→%d, barPos %d→%d)",
					changed, tt.wantStateChanges,
					tickBefore, r.tick, posBefore, r.barPos)
			}
		})
	}
}

// elementsOf builds a progress element set for test tables.
func elementsOf(elems ...ProgressElement) progressElements {
	return newProgressElements(elems)
}

// waveOf renders the first wave frame of a fresh renderer, w columns wide.
func waveOf(w int) string {
	return newCIRenderer(io.Discard, 0, false).renderWaveLine(w, true)
}

var mpbSpeedRe = regexp.MustCompile(`([0-9]+\.[0-9]{2}) (b|KiB|MiB|GiB|TiB)/s`)

// hasNonZeroSpeed reports whether mpb output shows a non-zero EWMA speed.
func hasNonZeroSpeed(s string) bool {
	for _, m := range mpbSpeedRe.FindAllStringSubmatch(s, -1) {
		if m[1] != "0.00" {
			return true
		}
	}
	return false
}

func Test_newProgressElements(t *testing.T) {
	type args struct {
		elems []ProgressElement
	}
	tests := []struct {
		name string
		args args
		want progressElements
	}{
		{name: "bytes", args: args{elems: []ProgressElement{ProgressElementBytes}}, want: 1},
		{name: "elapsed", args: args{elems: []ProgressElement{ProgressElementElapsed}},
			want: 2},
		{name: "speed", args: args{elems: []ProgressElement{ProgressElementSpeed}}, want: 4},
		{name: "bar", args: args{elems: []ProgressElement{ProgressElementBar}}, want: 8},
		{name: "percent", args: args{elems: []ProgressElement{ProgressElementPercent}},
			want: 16},
		{name: "ETA", args: args{elems: []ProgressElement{ProgressElementETA}}, want: 32},
		{name: "no elements", args: args{}, want: 0},
		{
			name: "duplicates and order are ignored",
			args: args{elems: []ProgressElement{
				ProgressElementETA, ProgressElementBytes, ProgressElementETA,
			}},
			want: 33,
		},
		{
			name: "unknown values are ignored",
			args: args{elems: []ProgressElement{-1, ProgressElementETA + 1, ProgressElementBar}},
			want: 8,
		},
		{
			name: "all elements make the full line",
			args: args{elems: []ProgressElement{
				ProgressElementBytes, ProgressElementElapsed, ProgressElementSpeed,
				ProgressElementBar, ProgressElementPercent, ProgressElementETA,
			}},
			want: progressAllElements,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newProgressElements(tt.args.elems); got != tt.want {
				t.Errorf("newProgressElements() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_newProgressConfig(t *testing.T) {
	type args struct {
		opts []ProgressOption
	}
	buf := &bytes.Buffer{}
	tests := []struct {
		name string
		args args
		want progressConfig
	}{
		{name: "no options", args: args{}, want: progressConfig{}},
		{
			name: "name",
			args: args{opts: []ProgressOption{WithProgressName("pkg")}},
			want: progressConfig{opts: ProgressOptions{Name: "pkg"}},
		},
		{
			name: "total",
			args: args{opts: []ProgressOption{WithProgressTotal(42)}},
			want: progressConfig{opts: ProgressOptions{Total: 42}},
		},
		{
			name: "stderr",
			args: args{opts: []ProgressOption{WithProgressStderr(buf)}},
			want: progressConfig{opts: ProgressOptions{Stderr: buf}},
		},
		{
			name: "CI mode on",
			args: args{opts: []ProgressOption{WithProgressCIMode(true)}},
			want: progressConfig{opts: ProgressOptions{CIMode: ptrBool(true)}},
		},
		{
			name: "CI mode off",
			args: args{opts: []ProgressOption{WithProgressCIMode(false)}},
			want: progressConfig{opts: ProgressOptions{CIMode: ptrBool(false)}},
		},
		{
			name: "no progress bar",
			args: args{opts: []ProgressOption{WithProgressNoProgressBar(true)}},
			want: progressConfig{opts: ProgressOptions{NoProgressBar: true}},
		},
		{
			name: "reverse",
			args: args{opts: []ProgressOption{WithProgressReverse(true)}},
			want: progressConfig{opts: ProgressOptions{Reverse: true}},
		},
		{
			name: "refresh rate",
			args: args{opts: []ProgressOption{WithProgressRefreshRate(time.Second)}},
			want: progressConfig{opts: ProgressOptions{RefreshRate: time.Second}},
		},
		{
			name: "elements",
			args: args{opts: []ProgressOption{
				WithProgressElements(ProgressElementBar, ProgressElementBytes),
			}},
			want: progressConfig{
				elements: elementsOf(ProgressElementBytes, ProgressElementBar), custom: true,
			},
		},
		{
			name: "elements without arguments select an empty set",
			args: args{opts: []ProgressOption{WithProgressElements()}},
			want: progressConfig{custom: true},
		},
		{
			name: "last elements option wins",
			args: args{opts: []ProgressOption{
				WithProgressElements(ProgressElementBytes),
				WithProgressElements(ProgressElementETA),
			}},
			want: progressConfig{elements: elementsOf(ProgressElementETA), custom: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newProgressConfig(tt.args.opts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newProgressConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewProgress(t *testing.T) {
	type args struct {
		ctx  context.Context
		opts []ProgressOption
	}
	// stopped makes the CI render loop exit at once, so only the frames
	// driven by the test are printed.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	ciOut, noBarOut, noBytesOut := &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	addOut, writeOut := &bytes.Buffer{}, &bytes.Buffer{}
	// canceledOut collects the output of the mpb bar built on the already
	// canceled stopped context; live and ciLive are canceled by their cases
	// after the bar is built.
	canceledOut := &bytes.Buffer{}
	live, stopLive := context.WithCancel(context.Background())
	defer stopLive()
	ciLive, stopCILive := context.WithCancel(context.Background())
	defer stopCILive()
	elapsedRe := regexp.MustCompile(`[0-9]:[0-9]{2}:[0-9]{2}`)
	// mpb renders into the non-terminal buffers only with auto refresh.
	mpbContainerOptions = []mpb.ContainerOption{mpb.WithAutoRefresh()}
	defer func() { mpbContainerOptions = nil }()
	tests := []struct {
		name  string
		args  args
		want  ProgressBar
		check func(t *testing.T, got ProgressBar) // replaces want when set
	}{
		{
			name: "empty element set in CI mode renders nothing",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(true), WithProgressStderr(io.Discard),
				WithProgressElements(),
			}},
			want: noopProgressBar{},
		},
		{
			name: "unknown elements in mpb mode render nothing",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(io.Discard),
				WithProgressElements(-1),
			}},
			want: noopProgressBar{},
		},
		{
			name: "no progress bar wins over elements",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressNoProgressBar(true), WithProgressElements(ProgressElementBar),
			}},
			want: noopProgressBar{},
		},
		{
			name: "no elements option renders the full line",
			args: args{ctx: stopped, opts: []ProgressOption{
				WithProgressCIMode(true), WithProgressStderr(io.Discard),
			}},
			check: func(t *testing.T, got ProgressBar) {
				cp, ok := got.(*ciProgressBar)
				if !ok {
					t.Fatalf("NewProgress() = %T, want *ciProgressBar", got)
				}
				defer cp.Finish(nil)
				if cp.r.elements != progressAllElements {
					t.Errorf("elements = %b, want %b", cp.r.elements, progressAllElements)
				}
			},
		},
		{
			name: "CI mode switches from unknown to known size",
			args: args{ctx: stopped, opts: []ProgressOption{
				WithProgressCIMode(true), WithProgressStderr(ciOut), WithProgressTotal(-1),
				WithProgressElements(ProgressElementPercent, ProgressElementBytes),
			}},
			check: func(t *testing.T, got ProgressBar) {
				cp, ok := got.(*ciProgressBar)
				if !ok {
					t.Fatalf("NewProgress() = %T, want *ciProgressBar", got)
				}
				<-cp.r.done
				ciOut.Reset()
				cp.AddBytes(512)
				cp.r.frame()
				cp.r.frame()
				cp.SetTotal(1024)
				cp.r.frame()
				cp.Finish(nil)
				want := "512B\n512B/1.0K  50%\n512B/1.0K  50%\n"
				if s := ciOut.String(); s != want {
					t.Errorf("output = %q, want %q", s, want)
				}
			},
		},
		{
			name: "mpb without bar element draws no bar",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(noBarOut), WithProgressTotal(100),
				WithProgressRefreshRate(time.Millisecond),
				WithProgressElements(ProgressElementPercent, ProgressElementBytes),
			}},
			check: func(t *testing.T, got ProgressBar) {
				got.AddBytes(100)
				got.Finish(nil)
				s := noBarOut.String()
				if !strings.Contains(s, "100%") || !strings.Contains(s, " / ") ||
					strings.Contains(s, "|") {
					t.Errorf("output = %q, want percent and counters without a bar", s)
				}
			},
		},
		{
			name: "mpb without bytes element draws no counters",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(noBytesOut), WithProgressTotal(100),
				WithProgressRefreshRate(time.Millisecond),
				WithProgressElements(ProgressElementETA, ProgressElementBar,
					ProgressElementSpeed, ProgressElementElapsed),
			}},
			check: func(t *testing.T, got ProgressBar) {
				got.AddBytes(100)
				got.Finish(nil)
				s := noBytesOut.String()
				if !strings.Contains(s, "|") || !elapsedRe.MatchString(s) ||
					strings.Contains(s, " / ") || strings.Contains(s, "%") {
					t.Errorf("output = %q, want bar and elapsed time without counters", s)
				}
			},
		},
		{
			name: "mpb AddBytes feeds the EWMA speed",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(addOut),
				WithProgressRefreshRate(time.Millisecond),
			}},
			check: func(t *testing.T, got ProgressBar) {
				for i := 0; i < 12; i++ {
					time.Sleep(2 * time.Millisecond)
					got.AddBytes(1024)
				}
				got.Finish(nil)
				if s := addOut.String(); !hasNonZeroSpeed(s) {
					t.Errorf("output = %q, want a non-zero speed", s)
				}
			},
		},
		{
			name: "mpb WrapWriter feeds the EWMA speed",
			args: args{ctx: context.Background(), opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(writeOut),
				WithProgressRefreshRate(time.Millisecond),
			}},
			check: func(t *testing.T, got ProgressBar) {
				w := got.WrapWriter(io.Discard)
				for i := 0; i < 12; i++ {
					time.Sleep(2 * time.Millisecond)
					if _, err := w.Write(make([]byte, 1024)); err != nil {
						t.Fatalf("Write: %v", err)
					}
				}
				if err := w.Close(); err != nil {
					t.Errorf("Close() error = %v, want nil", err)
				}
				got.Finish(nil)
				if s := writeOut.String(); !hasNonZeroSpeed(s) {
					t.Errorf("output = %q, want a non-zero speed", s)
				}
			},
		},
		{
			name: "mpb on a canceled context renders nothing",
			args: args{ctx: stopped, opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(canceledOut), WithProgressTotal(100),
			}},
			check: func(t *testing.T, got ProgressBar) {
				if _, ok := got.(noopProgressBar); !ok {
					t.Errorf("NewProgress() = %T, want noopProgressBar", got)
				}
				exerciseBar(t, got)
				if s := canceledOut.String(); s != "" {
					t.Errorf("output = %q, want empty", s)
				}
			},
		},
		{
			name: "mpb bar canceled after creation stays safe",
			args: args{ctx: live, opts: []ProgressOption{
				WithProgressCIMode(false), WithProgressStderr(io.Discard), WithProgressTotal(100),
			}},
			check: func(t *testing.T, got ProgressBar) {
				mp, ok := got.(*mpbProgressBar)
				if !ok {
					t.Fatalf("NewProgress() = %T, want *mpbProgressBar", got)
				}
				stopLive()
				// Once the bar has stopped, mpb hands out no proxy reader any more.
				mp.bar.Wait()
				exerciseBar(t, got)
			},
		},
		{
			// The output is not checked: the render loop may still print
			// frames until it notices the canceled context.
			name: "CI on a canceled context stays safe",
			args: args{ctx: stopped, opts: []ProgressOption{
				WithProgressCIMode(true), WithProgressStderr(io.Discard), WithProgressTotal(100),
			}},
			check: func(t *testing.T, got ProgressBar) {
				if _, ok := got.(*ciProgressBar); !ok {
					t.Fatalf("NewProgress() = %T, want *ciProgressBar", got)
				}
				exerciseBar(t, got)
			},
		},
		{
			name: "CI canceled after start keeps the bar safe",
			args: args{ctx: ciLive, opts: []ProgressOption{
				WithProgressCIMode(true), WithProgressStderr(io.Discard), WithProgressTotal(100),
			}},
			check: func(t *testing.T, got ProgressBar) {
				cp, ok := got.(*ciProgressBar)
				if !ok {
					t.Fatalf("NewProgress() = %T, want *ciProgressBar", got)
				}
				got.AddBytes(50)
				stopCILive()
				<-cp.r.done
				exerciseBar(t, got)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewProgress(tt.args.ctx, tt.args.opts...)
			if tt.check != nil {
				tt.check(t, got)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewProgress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_ciRenderer_composeLine(t *testing.T) {
	type fields struct {
		elements progressElements
	}
	type args struct {
		cur int64
		now time.Time
	}
	start := time.Now()
	at := args{cur: 109 << 20, now: start.Add(time.Second)}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   string
	}{
		{name: "only bytes", fields: fields{elementsOf(ProgressElementBytes)}, args: at,
			want: "109M"},
		{name: "only elapsed", fields: fields{elementsOf(ProgressElementElapsed)}, args: at,
			want: "0:00:01"},
		{name: "only speed", fields: fields{elementsOf(ProgressElementSpeed)}, args: at,
			want: "[78.7M/s]"},
		{name: "only bar draws a full-width wave",
			fields: fields{elementsOf(ProgressElementBar)}, args: at, want: waveOf(ciBarWidth)},
		{name: "percent and ETA are hidden",
			fields: fields{elementsOf(ProgressElementPercent, ProgressElementETA)}, args: at,
			want: ""},
		{name: "bytes and bar",
			fields: fields{elementsOf(ProgressElementBar, ProgressElementBytes)},
			args:   at, want: "109M " + waveOf(ciBarWidth-5)},
		{name: "full line", fields: fields{progressAllElements}, args: at,
			want: "109M 0:00:01 [78.7M/s] " + waveOf(ciBarWidth-23)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, -1, false)
			r.elements = tt.fields.elements
			r.startedAt = start
			r.speedEWMA = 82523750
			if got := r.composeLine(tt.args.cur, tt.args.now); got != tt.want {
				t.Errorf("ciRenderer.composeLine() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_ciRenderer_printComposeIfDue(t *testing.T) {
	type fields struct {
		elements    progressElements
		lastCompose time.Time
	}
	type args struct {
		cur int64
		now time.Time
	}
	now := time.Now()
	at := args{cur: 512, now: now}
	tests := []struct {
		name        string
		fields      fields
		args        args
		wantPrinted bool
	}{
		{name: "first line prints", fields: fields{elements: progressAllElements},
			args: at, wantPrinted: true},
		{name: "line within a second is suppressed",
			fields: fields{
				elements: progressAllElements, lastCompose: now.Add(-999 * time.Millisecond),
			},
			args: at, wantPrinted: false},
		{name: "line a second later prints",
			fields: fields{elements: progressAllElements, lastCompose: now.Add(-time.Second)},
			args:   at, wantPrinted: true},
		{name: "empty line is not printed",
			fields: fields{elements: elementsOf(ProgressElementPercent, ProgressElementETA)},
			args:   at, wantPrinted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, -1, false)
			r.elements = tt.fields.elements
			r.lastCompose = tt.fields.lastCompose
			r.startedAt = now
			r.printComposeIfDue(tt.args.cur, tt.args.now)
			printed := buf.Len() > 0
			if printed != tt.wantPrinted {
				t.Errorf("printComposeIfDue printed=%v, want %v (buf=%q)",
					printed, tt.wantPrinted, buf.String())
			}
			wantLast := tt.fields.lastCompose
			if tt.wantPrinted {
				wantLast = tt.args.now
			}
			if !r.lastCompose.Equal(wantLast) {
				t.Errorf("lastCompose = %v, want %v", r.lastCompose, wantLast)
			}
		})
	}
}

func TestNewProgressBar(t *testing.T) {
	type args struct {
		ctx  context.Context
		opts ProgressOptions
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	live, stopLive := context.WithCancel(context.Background())
	defer stopLive()
	canceledOut := &bytes.Buffer{}
	// mpb renders into non-terminal writers only with auto refresh.
	mpbContainerOptions = []mpb.ContainerOption{mpb.WithAutoRefresh()}
	defer func() { mpbContainerOptions = nil }()
	tests := []struct {
		name  string
		args  args
		want  ProgressBar                         // nil accepts any bar
		check func(t *testing.T, got ProgressBar) // runs after want is compared
	}{
		{
			name: "mpb on a canceled context renders nothing",
			args: args{ctx: canceled, opts: ProgressOptions{
				Total: 100, Stderr: canceledOut, CIMode: ptrBool(false),
			}},
			want: noopProgressBar{},
			check: func(t *testing.T, got ProgressBar) {
				exerciseBar(t, got)
				if s := canceledOut.String(); s != "" {
					t.Errorf("output = %q, want empty", s)
				}
			},
		},
		{
			name: "mpb bar canceled after creation stays safe",
			args: args{ctx: live, opts: ProgressOptions{
				Total: 100, Stderr: io.Discard, CIMode: ptrBool(false),
			}},
			check: func(t *testing.T, got ProgressBar) {
				mp, ok := got.(*mpbProgressBar)
				if !ok {
					t.Fatalf("NewProgressBar() = %T, want *mpbProgressBar", got)
				}
				stopLive()
				// Once the bar has stopped, mpb hands out no proxy reader any more.
				mp.bar.Wait()
				exerciseBar(t, got)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewProgressBar(tt.args.ctx, tt.args.opts)
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewProgressBar() = %v, want %v", got, tt.want)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func Test_newProgress(t *testing.T) {
	type args struct {
		ctx context.Context
		cfg progressConfig
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	seamOut := &bytes.Buffer{}
	addBar := mpbAddBar
	defer func() { mpbAddBar = addBar }()
	// mpb renders into non-terminal writers only with auto refresh.
	mpbContainerOptions = []mpb.ContainerOption{mpb.WithAutoRefresh()}
	defer func() { mpbContainerOptions = nil }()
	mpbConfig := func(w io.Writer) progressConfig {
		return progressConfig{opts: ProgressOptions{
			Total: 10, Stderr: w, CIMode: ptrBool(false),
		}}
	}
	tests := []struct {
		name string
		args args
		want ProgressBar // nil accepts any bar
		// runs repeats the call, each time on a fresh child of args.ctx.
		runs int
		// beforeAdd runs between creating the mpb container and adding the
		// bar; cancel cancels the context of the current call.
		beforeAdd func(p *mpb.Progress, cancel context.CancelFunc)
		check     func(t *testing.T) // runs after all the calls
	}{
		{
			name: "canceled before the call starts no goroutines",
			args: args{ctx: canceled, cfg: mpbConfig(io.Discard)},
			want: noopProgressBar{},
			runs: 1,
		},
		{
			name: "canceled between container and bar renders nothing",
			args: args{ctx: context.Background(), cfg: mpbConfig(seamOut)},
			want: noopProgressBar{},
			runs: 1,
			beforeAdd: func(p *mpb.Progress, cancel context.CancelFunc) {
				cancel()
				// Waiting for the container to stop makes adding the bar fail.
				p.Shutdown()
			},
			check: func(t *testing.T) {
				if s := seamOut.String(); s != "" {
					t.Errorf("output = %q, want empty", s)
				}
			},
		},
		{
			name: "cancel concurrent with adding the bar never panics",
			args: args{ctx: context.Background(), cfg: mpbConfig(io.Discard)},
			runs: 200,
			beforeAdd: func(_ *mpb.Progress, cancel context.CancelFunc) {
				go cancel()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			for i := 0; i < tt.runs; i++ {
				ctx, cancel := context.WithCancel(tt.args.ctx)
				mpbAddBar = func(
					p *mpb.Progress, total int64, filler mpb.BarFiller,
					options ...mpb.BarOption,
				) (*mpb.Bar, error) {
					if tt.beforeAdd != nil {
						tt.beforeAdd(p, cancel)
					}
					return addBar(p, total, filler, options...)
				}
				got := newProgress(ctx, tt.args.cfg)
				if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
					t.Errorf("newProgress() = %v, want %v", got, tt.want)
				}
				exerciseBar(t, got)
				cancel()
			}
			mpbAddBar = addBar
			if tt.check != nil {
				tt.check(t)
			}
			waitGoroutines(t, before)
		})
	}
}

// exerciseBar calls every ProgressBar method the way a transfer does and
// fails unless all of them return within 5s; the wrappers must pass the data
// through unchanged. The results are checked only after all the methods have
// returned, so a late method never reports into a finished test.
func exerciseBar(t *testing.T, bar ProgressBar) {
	t.Helper()
	var read []byte
	var readErr, readCloseErr, writeErr, writeCloseErr error
	var written int
	var sink bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		bar.AddBytes(3)
		bar.SetTotal(10)
		r := bar.WrapReader(strings.NewReader("abc"))
		read, readErr = io.ReadAll(r)
		readCloseErr = r.Close()
		w := bar.WrapWriter(&sink)
		written, writeErr = w.Write([]byte("abc"))
		writeCloseErr = w.Close()
		bar.Finish(nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProgressBar methods did not return within 5s")
	}
	if readErr != nil || string(read) != "abc" {
		t.Errorf("WrapReader read %q, %v, want %q, nil", read, readErr, "abc")
	}
	if readCloseErr != nil {
		t.Errorf("WrapReader Close() error = %v, want nil", readCloseErr)
	}
	if writeErr != nil || written != 3 {
		t.Errorf("WrapWriter wrote %d, %v, want 3, nil", written, writeErr)
	}
	if writeCloseErr != nil {
		t.Errorf("WrapWriter Close() error = %v, want nil", writeCloseErr)
	}
	if s := sink.String(); s != "abc" {
		t.Errorf("WrapWriter passed %q, want %q", s, "abc")
	}
}

// waitGoroutines fails unless the number of goroutines drops back to at most
// want within 5s: mpb ends some of its goroutines only after Shutdown or Wait
// has returned.
func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			t.Errorf("goroutines = %d, want at most %d", runtime.NumGoroutine(), want)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
