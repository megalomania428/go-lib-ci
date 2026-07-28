// cspell:ignore rawhex stallsig syncfail badurl copyerr defaultdest isdir chmodfail
// cspell:ignore ctxmid cimode nofile nontty pvline waveline humanb fmtdur speedewma
// cspell:ignore EWMA closefail alives oneshot ciresume digesterr
package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// closeTmpFdViaProc scans /proc/self/fd for an open descriptor whose target
// contains name and closes it. Used on Linux to inject EBADF into Sync or
// Close on the temp file held by downloadOnce.
func closeTmpFdViaProc(name string) bool {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false
	}
	closed := false
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(link, name) {
			if fd, err := strconv.Atoi(e.Name()); err == nil {
				if unix.Close(fd) == nil {
					closed = true
				}
			}
		}
	}
	return closed
}

// eofClosesReader returns the buffered data without EOF, then on the next
// call closes any open fd whose path contains name and returns EOF.
type eofClosesReader struct {
	data   []byte
	pos    int
	closed bool
	name   string
}

func (r *eofClosesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		if !r.closed {
			r.closed = true
			closeTmpFdViaProc(r.name)
		}
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// callDownloadOnce invokes downloadOnce with the deterministic partial-file
// path derived the same way downloadWithRetry derives it, so tests exercise the
// production resume path without recomputing the name at every call site.
func callDownloadOnce(
	ctx context.Context, opts *DownloadOptions, url, dest string,
) error {
	return downloadOnce(ctx, opts, url, dest, partPath(opts, dest))
}

func Test_downloadOnceSyncError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd shutdown via /proc/self/fd is linux-only")
	}
	name := "syncfail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, name)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	opts := &DownloadOptions{
		Name:          name,
		NoProgressBar: true,
		StallTimeout:  -1,

		// Wrap the response body so that after the body has been fully read,
		// the temp file's fd is closed via /proc/self/fd, forcing tmp.Sync to
		// return EBADF.
		HTTPClient: fdRewritingClient(srv.Client(), name),

		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}

	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil {
		t.Fatalf("expected Sync error, got nil")
	}
}

func Test_downloadOnceCloseError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd shutdown via /proc/self/fd is linux-only")
	}
	name := "closefail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, name)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	// Override osOpenPart so that after Sync succeeds the fd is closed via
	// /proc/self/fd, causing the subsequent tmp.Close() to return EBADF.
	orig := osOpenPart
	t.Cleanup(func() { osOpenPart = orig })
	osOpenPart = func(pth string, truncate bool) (tempFile, error) {
		flag := os.O_WRONLY | os.O_CREATE
		if truncate {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_APPEND
		}
		f, err := os.OpenFile(pth, flag, 0o644)
		if err != nil {
			return nil, err
		}
		return &closeErrFile{File: f}, nil
	}

	opts := &DownloadOptions{
		Name:          name,
		NoProgressBar: true,
		StallTimeout:  -1,

		HTTPClient: srv.Client(), BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}

	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil {
		t.Fatalf("expected Close error, got nil")
	}
}

// closeErrFile wraps *os.File and closes the underlying fd via /proc/self/fd
// after Sync succeeds, so that the subsequent Close returns EBADF.
type closeErrFile struct {
	*os.File
}

func (f *closeErrFile) Sync() error {
	if err := f.File.Sync(); err != nil {
		return err
	}
	closeTmpFdViaProc(f.Name())
	return nil
}

// fdRewritingClient returns an http.Client whose response bodies are wrapped
// in eofClosesReader so that the temp file fd is closed once the body has
// been fully read.
func fdRewritingClient(base *http.Client, name string) *http.Client {
	c := *base
	c.Transport = &fdClosingTransport{base: base.Transport, name: name}
	return &c
}

type fdClosingTransport struct {
	base http.RoundTripper
	name string
}

func (t *fdClosingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	orig := resp.Body
	resp.Body = &wrappedBody{r: &eofClosesReader{data: readAllClose(orig),
		name: t.name}}
	return resp, nil
}

type wrappedBody struct {
	r io.Reader
}

func (b *wrappedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *wrappedBody) Close() error               { return nil }

func readAllClose(r io.ReadCloser) []byte {
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		panic(fmt.Sprintf("readAllClose: %v", err))
	}
	return data
}

// tmpUnlinkingClient returns an http.Client whose response bodies, once fully
// read, trigger removal of any temp files matching ".<name>.*" inside
// dirToClean. The temp file's fd stays open so Sync/Close succeed, but the
// subsequent os.Chmod on tmpPath fails with ENOENT.
func tmpUnlinkingClient(base *http.Client, name, dirToClean string) *http.Client {
	c := *base
	c.Transport = &tmpUnlinkTransport{
		base: base.Transport, name: name, dir: dirToClean,
	}
	return &c
}

type tmpUnlinkTransport struct {
	base http.RoundTripper
	name string
	dir  string
}

func (t *tmpUnlinkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	orig := resp.Body
	resp.Body = &wrappedBody{r: &eofUnlinksReader{
		data: readAllClose(orig), dir: t.dir, name: t.name,
	}}
	return resp, nil
}

type eofUnlinksReader struct {
	data     []byte
	pos      int
	dir      string
	name     string
	unlinked bool
}

func (r *eofUnlinksReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		if !r.unlinked {
			r.unlinked = true
			removeTempFiles(r.dir, r.name)
		}
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func removeTempFiles(dir, name string) {
	prefix := "." + name + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Test_downloadOnceChmodError covers the os.Chmod failure branch in
// downloadOnce: after the temp file has been synced and closed, its directory
// entry is removed so Chmod(tmpPath) returns ENOENT.
func Test_downloadOnceChmodError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("temp-file removal trick is linux-specific")
	}
	name := "chmodfail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, name)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	opts := &DownloadOptions{
		Name:           name,
		NoProgressBar:  true,
		StallTimeout:   -1,
		HTTPClient:     tmpUnlinkingClient(srv.Client(), name, destDir),
		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}
	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil {
		t.Fatalf("expected chmod error, got nil")
	}
}

func Test_downloadWithRetry(t *testing.T) {
	payload := []byte("data")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()
	destDir := t.TempDir()

	type args struct {
		ctx  context.Context
		opts *DownloadOptions
		url  string
		dest string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name: "downloads and verifies digest",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "ok.bin",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,

					WantDigest: digest, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
				},
				url:  srv.URL,
				dest: filepath.Join(destDir, "ok.bin"),
			},
			wantErr: false,
		},
		{
			name: "empty url errors",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "bad.bin",
					NoProgressBar: true,
					StallTimeout:  -1,

					Stderr: io.Discard, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
				},
				url:  "",
				dest: filepath.Join(destDir, "bad.bin"),
			},
			wantErr: true,
			errSub:  "empty download URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := downloadWithRetry(
				tt.args.ctx, tt.args.opts, tt.args.url, tt.args.dest,
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("downloadWithRetry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("downloadWithRetry() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func Test_downloadOnce(t *testing.T) {
	payload := []byte("payload body")
	sum := sha256.Sum256(payload)
	goodDigest := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	nonOkSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}),
	)
	defer nonOkSrv.Close()

	destDir := t.TempDir()

	type args struct {
		ctx  context.Context
		opts *DownloadOptions
		url  string
		dest string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name: "successful download writes file",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "ok.bin",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
					WantDigest:    goodDigest,
				},
				url:  srv.URL,
				dest: filepath.Join(destDir, "ok.bin"),
			},
			wantErr: false,
		},
		{
			name: "non-200 status errors",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "forbidden.bin",
					HTTPClient:    nonOkSrv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				url:  nonOkSrv.URL,
				dest: filepath.Join(destDir, "forbidden.bin"),
			},
			wantErr: true,
			errSub:  "403 Forbidden",
		},
		{
			name: "digest mismatch errors",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "mis.bin",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
					WantDigest:    strings.Repeat("a", 64),
				},
				url:  srv.URL,
				dest: filepath.Join(destDir, "mis.bin"),
			},
			wantErr: true,
			errSub:  "digest mismatch",
		},
		{
			name: "open partial failure errors",
			args: args{
				ctx: context.Background(),
				opts: &DownloadOptions{
					Name:          "blocker_under_file",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				url:  srv.URL,
				dest: filepath.Join(destDir, "blocker_under_file", "child"),
			},
			wantErr: true,
			errSub:  "open partial file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.args.opts != nil && tt.args.opts.Name == "blocker_under_file" {
				blocker := filepath.Join(destDir, "blocker_under_file")
				if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
					t.Fatalf("seed blocker: %v", err)
				}
			}
			err := callDownloadOnce(
				tt.args.ctx, tt.args.opts, tt.args.url, tt.args.dest,
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("downloadOnce() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("downloadOnce() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func Test_downloadOnceRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
		}),
	)
	srv.Close() // closed server -> connection refused on download
	destDir := t.TempDir()
	opts := &DownloadOptions{
		Name:          "closed.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
	}
	err := callDownloadOnce(
		context.Background(), opts, srv.URL,
		filepath.Join(destDir, "closed.bin"),
	)
	if err == nil {
		t.Fatalf("expected transport error from closed server")
	}
}

func Test_downloadOnceBadURL(t *testing.T) {
	destDir := t.TempDir()
	opts := &DownloadOptions{
		Name:          "badurl.bin",
		NoProgressBar: true,
		StallTimeout:  -1,
	}
	err := callDownloadOnce(
		context.Background(), opts, "ht!tp://invalid url with spaces",
		filepath.Join(destDir, "badurl.bin"),
	)
	if err == nil {
		t.Fatalf("expected new request error")
	}
}

func Test_downloadOnceCopyError(t *testing.T) {
	destDir := t.TempDir()
	opts := &DownloadOptions{
		Name:          "copyerr.bin",
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
		HTTPClient: &http.Client{
			Transport: &failingBodyTransport{},
		},
	}
	err := callDownloadOnce(
		context.Background(), opts,
		"http://example/asset", // DevSkim: ignore DS137138
		filepath.Join(destDir, "copyerr.bin"),
	)
	if err == nil {
		t.Fatalf("expected copy error from failing body transport")
	}
}

type failingBodyTransport struct{}

func (t *failingBodyTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(&errReader{err: errors.New("copy failed")}),
	}, nil
}

func Test_downloadOnceRenameFailure(t *testing.T) {
	payload := []byte("rename me")
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()
	destDir := t.TempDir()
	// dest exists as a directory -> os.Rename returns EISDIR.
	dest := filepath.Join(destDir, "isdir.bin")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	opts := &DownloadOptions{
		Name:          "isdir.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
	}
	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil {
		t.Fatalf("expected rename error, got nil")
	}
	if !strings.Contains(err.Error(), "rename") {
		// On some platforms rename over an existing dir may return a
		// different errno; the important check is that an error surfaced.
		t.Logf(`rename error does not contain "rename": %v`, err)
	}
}

// Test_downloadOnceParentCtxCanceledDuringCopy exercises the `ctx.Err() != nil`
// branch inside downloadOnce: copy aborts because the parent ctx (and thus
// dlCtx) is canceled mid-transfer.
func Test_downloadOnceParentCtxCanceledDuringCopy(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte{'x'})
				f.Flush()
			}
			<-release
		}),
	)
	defer srv.Close()
	defer close(release)
	destDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	opts := &DownloadOptions{
		Name:          "ctxmid.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
	}
	err := callDownloadOnce(
		ctx, opts, srv.URL, filepath.Join(destDir, "ctxmid.bin"),
	)
	if err == nil {
		t.Fatalf("expected ctx-canceled error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func Test_downloadOnceTransferStalledShortCircuit(t *testing.T) {
	name := "stallsig.bin"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte{'x'})
				f.Flush()
			}
			<-release
		}),
	)
	defer srv.Close()
	defer close(release)
	destDir := t.TempDir()
	opts := &DownloadOptions{
		Name:          name,
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  100 * time.Millisecond,
		StallLimit:    1024 * 1024,
		Stderr:        io.Discard,
	}
	err := callDownloadOnce(
		context.Background(), opts, srv.URL,
		filepath.Join(destDir, name),
	)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stalled error, got %v", err)
	}
}

func TestFetchURL(t *testing.T) {
	payload := []byte("fetch url payload")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	t.Run("downloads plain http url", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "plain.bin")
		got, err := FetchURL(context.Background(), srv.URL, dest, DownloadOptions{
			NoProgressBar: true,
			StallTimeout:  -1,

			HTTPClient: srv.Client(),
			WantDigest: digest, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
		})
		if err != nil {
			t.Fatalf("FetchURL: %v", err)
		}
		if got != dest {
			t.Errorf("FetchURL returned %s, want %s", got, dest)
		}
		body, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read dest: %v", err)
		}
		if !bytes.Equal(body, payload) {
			t.Errorf("payload mismatch: %q", body)
		}
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("unexpected mode: %v", info.Mode())
		}
	})

	t.Run("default client and name fallback", func(t *testing.T) {
		// No HTTPClient and no Name: exercises applyDownloadDefaults building
		// a real client and FetchURL deriving Name from the dest base name.
		dest := filepath.Join(t.TempDir(), "derived.bin")
		got, err := FetchURL(context.Background(), srv.URL, dest, DownloadOptions{
			NoProgressBar: true,
			StallTimeout:  -1, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
		})
		if err != nil {
			t.Fatalf("FetchURL: %v", err)
		}
		if got != dest {
			t.Errorf("FetchURL returned %s, want %s", got, dest)
		}
	})

	t.Run("zero StallTimeout and MaxRetriesTime use defaults", func(t *testing.T) {
		// StallTimeout and MaxRetriesTime left at zero: exercises the
		// applyDownloadDefaults branches that fall back to
		// defaultStallTimeout and defaultMaxRetriesTime.
		dest := filepath.Join(t.TempDir(), "defaults.bin")
		got, err := FetchURL(context.Background(), srv.URL, dest, DownloadOptions{
			NoProgressBar: true,
			HTTPClient:    srv.Client(),
		})
		if err != nil {
			t.Fatalf("FetchURL: %v", err)
		}
		if got != dest {
			t.Errorf("FetchURL returned %s, want %s", got, dest)
		}
	})

	t.Run("download failure surfaces error", func(t *testing.T) {
		failSrv := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}),
		)
		defer failSrv.Close()
		dest := filepath.Join(t.TempDir(), "fail.bin")
		_, err := FetchURL(context.Background(), failSrv.URL, dest, DownloadOptions{
			NoProgressBar: true,
			StallTimeout:  -1,

			HTTPClient: failSrv.Client(),
			Stderr:     io.Discard, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
		})
		if err == nil || !strings.Contains(err.Error(), "500 Internal Server Error") {
			t.Fatalf("expected download failure error, got %v", err)
		}
	})

	t.Run("custom headers are applied", func(t *testing.T) {
		var gotHeader string
		hdrSrv := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("X-Test")
				_, _ = w.Write(payload)
			}),
		)
		defer hdrSrv.Close()
		dest := filepath.Join(t.TempDir(), "hdr.bin")
		_, err := FetchURL(context.Background(), hdrSrv.URL, dest, DownloadOptions{
			NoProgressBar: true,
			StallTimeout:  -1,

			HTTPClient: hdrSrv.Client(),
			SetHeaders: func(req *http.Request) {
				req.Header.Set("X-Test", "yes")
			}, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
		})
		if err != nil {
			t.Fatalf("FetchURL: %v", err)
		}
		if gotHeader != "yes" {
			t.Errorf("SetHeaders not applied, got %q", gotHeader)
		}
	})

	t.Run("empty url rejected", func(t *testing.T) {
		_, err := FetchURL(context.Background(), "", "/tmp/x", DownloadOptions{})
		if err == nil || !strings.Contains(err.Error(), "url required") {
			t.Fatalf("expected url required error, got %v", err)
		}
	})

	t.Run("empty dest rejected", func(t *testing.T) {
		_, err := FetchURL(context.Background(), srv.URL, "", DownloadOptions{})
		if err == nil || !strings.Contains(err.Error(), "dest required") {
			t.Fatalf("expected dest required error, got %v", err)
		}
	})
}

func Test_copyWithProgress(t *testing.T) {
	type args struct {
		opts  *DownloadOptions
		src   io.Reader
		total int64
	}
	tests := []struct {
		name    string
		args    args
		wantDst string
		wantErr bool
	}{
		{
			name: "no progress bar copies bytes",
			args: args{
				opts:  &DownloadOptions{NoProgressBar: true},
				src:   strings.NewReader("plain copy"),
				total: 10,
			},
			wantDst: "plain copy",
			wantErr: false,
		},
		{
			name: "progress bar known total copies bytes",
			args: args{
				opts: &DownloadOptions{
					Name:          "p.bin",
					NoProgressBar: false,
					Stderr:        io.Discard,
				},
				src:   strings.NewReader("with bar"),
				total: 8,
			},
			wantDst: "with bar",
			wantErr: false,
		},
		{
			name: "progress bar unknown total copies bytes",
			args: args{
				opts: &DownloadOptions{
					Name:          "p.bin",
					NoProgressBar: false,
					Stderr:        io.Discard,
				},
				src:   strings.NewReader("unknown size"),
				total: -1,
			},
			wantDst: "unknown size",
			wantErr: false,
		},
		{
			name: "reader error surfaces without progress bar",
			args: args{
				opts:  &DownloadOptions{NoProgressBar: true},
				src:   &errReader{err: errors.New("boom")},
				total: 5,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &bytes.Buffer{}
			err := copyWithProgress(context.Background(), tt.args.opts, dst, tt.args.src,
				tt.args.total, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("copyWithProgress() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && dst.String() != tt.wantDst {
				t.Errorf("copyWithProgress() dst = %q, want %q", dst.String(), tt.wantDst)
			}
		})
	}
}

type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

type errReader struct{ err error }

func (r *errReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 0, r.err
}

func Test_watchdog(t *testing.T) {
	type args struct {
		ctx     context.Context
		stalled *atomic.Bool
		counter *atomic.Int64
		window  time.Duration
		limit   int64
		name    string
	}
	tests := []struct {
		name        string
		args        args
		wantStalled bool
		wantStderr  string
	}{
		{
			name: "context already canceled returns silently",
			args: args{
				ctx: func() context.Context {
					c, cancel := context.WithCancel(context.Background())
					cancel()
					return c
				}(),
				stalled: &atomic.Bool{},
				counter: &atomic.Int64{},
				window:  time.Second,
				limit:   1,
				name:    "x.bin",
			},
			wantStalled: false,
			wantStderr:  "",
		},
		{
			name: "stall detected aborts transfer",
			args: args{
				ctx:     context.Background(),
				stalled: &atomic.Bool{},
				counter: &atomic.Int64{},
				window:  20 * time.Millisecond,
				limit:   1024,
				name:    "y.bin",
			},
			wantStalled: true,
			wantStderr:  "transfer stalled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(tt.args.ctx)
			defer cancel()
			stderr := &bytes.Buffer{}
			watchdog(
				ctx, cancel, tt.args.stalled, tt.args.counter,
				tt.args.window, tt.args.limit, stderr, tt.args.name,
			)
			if tt.args.stalled.Load() != tt.wantStalled {
				t.Errorf("stalled = %v, want %v", tt.args.stalled.Load(), tt.wantStalled)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("watchdog stderr = %q, want substring %q", stderr.String(),
					tt.wantStderr)
			}
		})
	}
}

// Test_watchdogProgressUpdates exercises the branch in which throughput stays
// above StallLimit so the watchdog keeps moving prev forward and exits only
// once the context is canceled.
func Test_watchdogProgressUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stalled := &atomic.Bool{}
	var counter atomic.Int64
	stderr := &bytes.Buffer{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				counter.Add(1024 * 1024) // 1 MiB >> StallLimit (1024 B) to keep watchdog happy
			}
		}
	}()

	time.AfterFunc(80*time.Millisecond, cancel)
	watchdog(
		ctx, cancel, stalled, &counter,
		15*time.Millisecond, 1024, stderr, "z.bin",
	)
	<-done
	if stalled.Load() {
		t.Errorf("watchdog must not flag stall while throughput is high")
	}
}

func Test_countingReader_Read(t *testing.T) {
	type args struct {
		r io.Reader
		p []byte
	}
	tests := []struct {
		name    string
		args    args
		want    int
		wantCnt int64
		wantErr bool
	}{
		{
			name:    "counts bytes read",
			args:    args{r: strings.NewReader("hello"), p: make([]byte, 5)},
			want:    5,
			wantCnt: 5,
			wantErr: false,
		},
		{
			name:    "zero-length read does not increment",
			args:    args{r: strings.NewReader("hello"), p: make([]byte, 0)},
			want:    0,
			wantCnt: 0,
			wantErr: false,
		},
		{
			name:    "underlying reader error propagates",
			args:    args{r: &errReader{err: errors.New("boom")}, p: make([]byte, 4)},
			want:    0,
			wantCnt: 0,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var total atomic.Int64
			c := &countingReader{
				r:      tt.args.r,
				onRead: func(n int64) { total.Add(n) },
			}
			got, err := c.Read(tt.args.p)
			if (err != nil) != tt.wantErr {
				t.Errorf("countingReader.Read() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("countingReader.Read() = %v, want %v", got, tt.want)
			}
			if total.Load() != tt.wantCnt {
				t.Errorf("counter = %d, want %d", total.Load(), tt.wantCnt)
			}
		})
	}
}

func Test_retry(t *testing.T) {
	type args struct {
		ctxFn  func() context.Context
		opts   *DownloadOptions
		action string
		fn     func(context.Context) error
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name: "succeeds on first attempt",
			args: args{
				ctxFn: context.Background,
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				action: "ok",
				fn:     func(context.Context) error { return nil },
			},
			wantErr: false,
		},
		{
			name: "succeeds after transient failures",
			args: args{
				ctxFn: context.Background,
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				action: "retry-ok",
				fn: func() func(context.Context) error {
					calls := 0
					return func(context.Context) error {
						calls++
						if calls < 3 {
							return errors.New("transient")
						}
						return nil
					}
				}(),
			},
			wantErr: false,
		},
		{
			name: "exhausts budget and returns last error",
			args: args{
				ctxFn: context.Background,
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Millisecond, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				action: "always-fail",
				fn:     func(context.Context) error { return errors.New("hard fail") },
			},
			wantErr: true,
			errSub:  "hard fail",
		},
		{
			name: "already canceled context aborts before first call",
			args: args{
				ctxFn: func() context.Context {
					c, cancel := context.WithCancel(context.Background())
					cancel()
					return c
				},
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				action: "canceled",
				fn:     func(context.Context) error { return errors.New("must not run") },
			},
			wantErr: true,
			errSub:  context.Canceled.Error(),
		},
		{
			name: "context canceled during backoff sleep surfaces",
			args: args{
				ctxFn: func() context.Context {
					c, cancel := context.WithCancel(context.Background())
					time.AfterFunc(20*time.Millisecond, cancel)
					return c
				},
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Minute, BackoffInitial: time.Minute,
						BackoffStep: time.Minute, BackoffCap: time.Minute,
					},
				},
				action: "cancel-during-sleep",
				fn:     func(context.Context) error { return errors.New("again") },
			},
			wantErr: true,
			errSub:  context.Canceled.Error(),
		},
		{
			name: "negative MaxRetriesTime disables retries",
			args: args{
				ctxFn: context.Background,
				opts: &DownloadOptions{
					Stderr: io.Discard, BackoffOptions: BackoffOptions{
						MaxRetriesTime: -1, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				action: "small-max",
				fn:     func(context.Context) error { return errors.New("fail") },
			},
			wantErr: true,
			errSub:  "fail",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := retry(tt.args.ctxFn(), tt.args.opts, tt.args.action, tt.args.fn)
			if (err != nil) != tt.wantErr {
				t.Errorf("retry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("retry() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func TestRetry(t *testing.T) {
	type args struct {
		ctxFn func() context.Context
		opts  RetryOptions
		fn    func(*int) func(context.Context) error
	}
	tests := []struct {
		name       string
		args       args
		wantErr    bool
		errSub     string
		wantCalls  int
		wantLogSub string
	}{
		{
			name: "succeeds on first attempt applies nil Stderr default",
			args: args{
				ctxFn: context.Background,
				opts:  RetryOptions{Name: "ok"},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						return nil
					}
				},
			},
			wantErr:   false,
			wantCalls: 1,
		},
		{
			name: "succeeds after transient failures logs new format",
			args: args{
				ctxFn: context.Background,
				opts: RetryOptions{
					Name: "retry-ok", BackoffOptions: BackoffOptions{MaxRetriesTime: time.Second,
						BackoffInitial: time.Millisecond, BackoffStep: time.Millisecond,
						BackoffCap: time.Millisecond},
				},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						if *calls < 3 {
							return errors.New("transient")
						}
						return nil
					}
				},
			},
			wantErr:    false,
			wantCalls:  3,
			wantLogSub: "==> retry-ok failed: transient, sleep 1ms, elapsed 1ms of 1s",
		},
		{
			name: "budget bounds the number of attempts",
			args: args{
				ctxFn: context.Background,
				// A 3ms budget with a flat 1ms backoff allows three sleeps
				// (waited 1ms, 2ms, 3ms) before the fourth would exceed it,
				// so the function is invoked four times in total.
				opts: RetryOptions{
					Name: "defaults", BackoffOptions: BackoffOptions{
						MaxRetriesTime: 3 * time.Millisecond, BackoffInitial: time.Millisecond,
						BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
					},
				},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						return errors.New("hard fail")
					}
				},
			},
			wantErr:   true,
			errSub:    "hard fail",
			wantCalls: 4,
		},
		{
			name: "negative MaxRetriesTime disables retries",
			args: args{
				ctxFn: context.Background,
				opts: RetryOptions{
					Name: "SleepMax", BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
				},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						return errors.New("boom")
					}
				},
			},
			wantErr:   true,
			errSub:    "boom",
			wantCalls: 1,
		},
		{
			name: "already canceled context aborts before first call",
			args: args{
				ctxFn: func() context.Context {
					c, cancel := context.WithCancel(context.Background())
					cancel()
					return c
				},
				opts: RetryOptions{
					Name: "canceled", BackoffOptions: BackoffOptions{MaxRetriesTime: time.Second,
						BackoffInitial: time.Millisecond, BackoffStep: time.Millisecond,
						BackoffCap: time.Millisecond},
				},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						return errors.New("must not run")
					}
				},
			},
			wantErr:   true,
			errSub:    context.Canceled.Error(),
			wantCalls: 0,
		},
		{
			name: "context canceled during backoff sleep surfaces",
			args: args{
				ctxFn: func() context.Context {
					c, cancel := context.WithCancel(context.Background())
					time.AfterFunc(20*time.Millisecond, cancel)
					return c
				},
				opts: RetryOptions{
					Name: "cancel-during-sleep", BackoffOptions: BackoffOptions{
						MaxRetriesTime: time.Minute, BackoffInitial: time.Minute,
						BackoffStep: time.Minute, BackoffCap: time.Minute,
					},
				},
				fn: func(calls *int) func(context.Context) error {
					return func(context.Context) error {
						*calls++
						return errors.New("again")
					}
				},
			},
			wantErr: true,
			errSub:  context.Canceled.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			opts := tt.args.opts
			if opts.Stderr == nil && tt.wantLogSub != "" {
				opts.Stderr = &log
			}
			calls := 0
			err := Retry(tt.args.ctxFn(), opts, tt.args.fn(&calls))
			if (err != nil) != tt.wantErr {
				t.Errorf("Retry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("Retry() error = %v, want substring %q", err, tt.errSub)
			}
			if tt.wantCalls > 0 && calls != tt.wantCalls {
				t.Errorf("Retry() calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantLogSub != "" && !strings.Contains(log.String(), tt.wantLogSub) {
				t.Errorf("Retry() log = %q, want substring %q", log.String(),
					tt.wantLogSub)
			}
		})
	}
}

func Test_sha256File(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "data")
	payload := []byte("hash me")
	if err := os.WriteFile(existingPath, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	missingPath := filepath.Join(dir, "missing")

	type args struct {
		path string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "computes digest of file contents",
			args:    args{path: existingPath},
			want:    want,
			wantErr: false,
		},
		{
			name:    "missing file errors",
			args:    args{path: missingPath},
			want:    "",
			wantErr: true,
		},
		{
			name:    "unreadable source surfaces io error",
			args:    args{path: "/proc/self/mem"},
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sha256File(tt.args.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("sha256File() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("sha256File() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_fileExists(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "exec")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missingPath := filepath.Join(dir, "missing")

	type args struct {
		path string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "executable file returns true",
			args: args{path: execPath},
			want: true,
		},
		{
			name: "non-executable file returns true",
			args: args{path: dataPath},
			want: true,
		},
		{
			name: "directory returns false",
			args: args{path: subDir},
			want: false,
		},
		{
			name: "missing path returns false",
			args: args{path: missingPath},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileExists(tt.args.path); got != tt.want {
				t.Errorf("fileExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewHTTPClient(t *testing.T) {
	tests := []struct {
		name           string
		connectTimeout time.Duration
		transport      http.RoundTripper
	}{
		{name: "default timeout", connectTimeout: 5 * time.Second},
		{name: "zero timeout still produces a client", connectTimeout: 0},
		{name: "non-Transport default does not panic", connectTimeout: 5 * time.Second,
			transport: stubTransport{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.transport != nil {
				orig := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = orig })
				http.DefaultTransport = tt.transport
			}
			got := NewHTTPClient(tt.connectTimeout)
			if got == nil {
				t.Fatalf("NewHTTPClient() returned nil")
			}
			tr, ok := got.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("expected *http.Transport, got %T", got.Transport)
			}
			if tr.TLSHandshakeTimeout != tt.connectTimeout {
				t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout,
					tt.connectTimeout)
			}
			if tr.ResponseHeaderTimeout != tt.connectTimeout {
				t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout,
					tt.connectTimeout)
			}
			if tr.DialContext == nil {
				t.Errorf("DialContext must be set")
			}
			if !tr.DisableKeepAlives {
				t.Errorf("DisableKeepAlives = false, want true")
			}
		})
	}
}

func TestNewHTTPClientNoKeepAlive(t *testing.T) {
	c := NewHTTPClient(5 * time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
}

func Test_useCIProgress(t *testing.T) {
	trueVal, falseVal := true, false
	t.Run("CIMode forced true", func(t *testing.T) {
		if !useCIProgress(&DownloadOptions{CIMode: &trueVal, Stderr: os.Stdout}) {
			t.Error("expected true when CIMode=true")
		}
	})
	t.Run("CIMode forced false", func(t *testing.T) {
		if useCIProgress(&DownloadOptions{CIMode: &falseVal, Stderr: os.Stdout}) {
			t.Error("expected false when CIMode=false")
		}
	})
	t.Run("non-file Stderr is CI", func(t *testing.T) {
		if !useCIProgress(&DownloadOptions{Stderr: &bytes.Buffer{}}) {
			t.Error("expected true for non-*os.File Stderr")
		}
	})
	t.Run("os.File non-tty is CI", func(t *testing.T) {
		// /dev/null is a file but not a terminal.
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Skip("cannot open /dev/null:", err)
		}
		defer f.Close()
		if !useCIProgress(&DownloadOptions{Stderr: f}) {
			t.Error("expected true for non-tty os.File")
		}
	})
}

func Test_downloadOnce_ciProgress(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "out.bin")
	trueVal := true
	opts := &DownloadOptions{
		Name:       "out.bin",
		FileMode:   0o644,
		HTTPClient: srv.Client(),
		CIMode:     &trueVal,
		Stderr:     io.Discard,
		Stdout:     io.Discard,
	}
	if err := callDownloadOnce(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadOnce CI path: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch")
	}
}

func Test_downloadOnce_ciProgress_unknownTotal(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length – triggers unknown-total wave path in ciRenderer.
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "out.bin")
	trueVal := true
	opts := &DownloadOptions{
		Name:       "out.bin",
		FileMode:   0o644,
		HTTPClient: srv.Client(),
		CIMode:     &trueVal,
		Stderr:     io.Discard,
		Stdout:     io.Discard,
	}
	if err := callDownloadOnce(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadOnce CI unknown-total: %v", err)
	}
}

func Test_ciRenderer_doubleStart(_ *testing.T) {
	r := newCIRenderer(io.Discard, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	r.Start(ctx) // second call is a no-op
	cancel()
	r.Stop()
}

func Test_ciRenderer_SetTotal(t *testing.T) {
	r := newCIRenderer(io.Discard, -1)
	r.SetTotal(1024)
	if got := r.total.Load(); got != 1024 {
		t.Errorf("SetTotal: got %d, want 1024", got)
	}
}

func Test_ciRenderer_AddBytes(t *testing.T) {
	r := newCIRenderer(io.Discard, 0)
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
	r := newCIRenderer(w, 0)
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
	r := newCIRenderer(buf, 1000)
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
	r := newCIRenderer(io.Discard, 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Drive barPos to the right boundary and back.
	for i := 0; i < ciBarWidth*2; i++ {
		r.renderWaveLine(true)
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
	r := newCIRenderer(io.Discard, 0)
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
		r2 := newCIRenderer(io.Discard, 0)
		cr := &countingReader{r: strings.NewReader("x"), onRead: r2.AddBytes}
		buf := make([]byte, 0)
		_, _ = cr.Read(buf)
		if r2.counter.Load() != 0 {
			t.Errorf("counter must stay 0 on zero-length read")
		}
	})
	t.Run("error propagates", func(t *testing.T) {
		r3 := newCIRenderer(io.Discard, 0)
		cr := &countingReader{r: &errReader{err: errors.New("ci-boom")}, onRead: r3.AddBytes}
		_, err := cr.Read(make([]byte, 4))
		if err == nil || !strings.Contains(err.Error(), "ci-boom") {
			t.Errorf("expected ci-boom error, got %v", err)
		}
	})
}

func Test_copyWithMpb(t *testing.T) {
	type args struct {
		opts  *DownloadOptions
		src   io.Reader
		total int64
	}
	tests := []struct {
		name    string
		args    args
		wantDst string
		wantErr bool
	}{
		{
			name: "known total copies bytes",
			args: args{
				opts: &DownloadOptions{
					Name:          "p.bin",
					NoProgressBar: false,
					Stderr:        io.Discard,
				},
				src:   strings.NewReader("with bar"),
				total: 8,
			},
			wantDst: "with bar",
		},
		{
			name: "unknown total copies bytes",
			args: args{
				opts: &DownloadOptions{
					Name:          "p.bin",
					NoProgressBar: false,
					Stderr:        io.Discard,
				},
				src:   strings.NewReader("unknown size"),
				total: -1,
			},
			wantDst: "unknown size",
		},
		{
			name: "reader error aborts bar and returns error",
			args: args{
				opts: &DownloadOptions{
					Name:          "p.bin",
					NoProgressBar: false,
					Stderr:        io.Discard,
				},
				src:   &errReader{err: errors.New("disk full")},
				total: 8,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &bytes.Buffer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := copyWithMpb(ctx, tt.args.opts, dst, tt.args.src, tt.args.total)
			if (err != nil) != tt.wantErr {
				t.Fatalf("copyWithMpb() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotDst := dst.String(); gotDst != tt.wantDst {
				t.Errorf("copyWithMpb() = %q, want %q", gotDst, tt.wantDst)
			}
		})
	}
}

func Test_copyWithProgressCIMode(t *testing.T) {
	type args struct {
		src io.Reader
	}
	tests := []struct {
		name    string
		args    args
		wantDst string
		wantErr bool
	}{
		{
			name:    "forwards bytes and updates ci renderer",
			args:    args{src: strings.NewReader("ci-progress-data")},
			wantDst: "ci-progress-data",
		},
		{
			name:    "reader error surfaces",
			args:    args{src: &errReader{err: errors.New("bang")}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciR := newCIRenderer(io.Discard, 16)
			opts := &DownloadOptions{NoProgressBar: false}
			dst := &bytes.Buffer{}
			err := copyWithProgress(
				context.Background(), opts, dst, tt.args.src, 16, ciR,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("copyWithProgress(ciR) error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotDst := dst.String(); gotDst != tt.wantDst {
				t.Errorf("copyWithProgress(ciR) = %q, want %q", gotDst, tt.wantDst)
			}
		})
	}
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
			got := newCIRenderer(out, tt.args.total)
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
			r := newCIRenderer(io.Discard, 100)
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
		wantContains string
		wantOmit     string
	}{
		{name: "stop before start does not panic",
			startFirst: false, total: 0, counter: -1, lastBar: 0},
		{name: "stop after full download prints final 100 percent line",
			startFirst: true, total: 1024, counter: 1024, lastBar: 10, wantContains: "100%"},
		{name: "stop after partial download does not print 100 percent",
			startFirst: true, total: 1024, counter: 512, lastBar: 10, wantOmit: "100%"},
		{name: "stop with zero total prints no final line",
			startFirst: true, total: 0, counter: -1, lastBar: 10, wantOmit: "100%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, tt.total)
			if tt.startFirst {
				ctx, cancel := context.WithCancel(context.Background())
				r.Start(ctx)
				defer cancel()
			}
			if tt.startFirst {
				r.mu.Lock()
				r.startedAt = time.Now()
				r.lastBar = tt.lastBar
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
		})
	}
}

// Test_ciRenderer_doubleStop exercises the `case <-r.stop:` branch in Stop:
// the stop channel is already closed by the first call, so the second call
// must not attempt to close it again (which would panic).
func Test_ciRenderer_doubleStop(_ *testing.T) {
	r := newCIRenderer(io.Discard, 0)
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
			r := newCIRenderer(io.Discard, 0)
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
			r := newCIRenderer(buf, tt.total)
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
			r := newCIRenderer(io.Discard, 0)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			r := newCIRenderer(buf, tt.args.total)
			r.startedAt = now
			r.lastBar = tt.fields.lastBar
			r.lastPrint = tt.fields.lastPrint
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

func Test_ciRenderer_renderHashLine(t *testing.T) {
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
			r := newCIRenderer(io.Discard, tt.args.total)
			r.startedAt = time.Now()
			got := r.renderHashLine(tt.args.cur, tt.args.total)
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("ciRenderer.renderHashLine() = %q, want substring %q",
					got, tt.wantContains)
			}
		})
	}
}

func Test_ciRenderer_pvLine(t *testing.T) {
	type fields struct {
		barWidth  int
		speedEWMA float64
	}
	type args struct {
		cur   int64
		total int64
	}
	tests := []struct {
		name           string
		fields         fields
		args           args
		wantFilledGt0  bool
		wantPct        float64
		wantContains   string
		wantOmitString string
	}{
		{
			name:          "mid transfer shows ETA",
			fields:        fields{barWidth: 0, speedEWMA: 0},
			args:          args{cur: 512, total: 1024},
			wantFilledGt0: true, wantPct: 50, wantContains: "ETA",
		},
		{
			name:          "complete hides ETA at 100 percent",
			fields:        fields{barWidth: 0, speedEWMA: 0},
			args:          args{cur: 1024, total: 1024},
			wantFilledGt0: false, wantPct: 100, wantOmitString: "ETA",
		},
		{
			name:          "overflow clamps to 100 percent",
			fields:        fields{barWidth: 0, speedEWMA: 0},
			args:          args{cur: 2048, total: 1024},
			wantFilledGt0: false, wantPct: 100, wantContains: "100%",
		},
		{
			name:          "positive EWMA keeps ETA",
			fields:        fields{barWidth: 0, speedEWMA: 1024},
			args:          args{cur: 256, total: 1024},
			wantFilledGt0: true, wantPct: 25, wantContains: "ETA",
		},
		{
			name:          "narrow bar still renders a line",
			fields:        fields{barWidth: 10, speedEWMA: 0},
			args:          args{cur: 5, total: 10},
			wantFilledGt0: false, wantPct: 50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, tt.args.total)
			r.barWidth = tt.fields.barWidth
			r.speedEWMA = tt.fields.speedEWMA
			r.startedAt = time.Now()
			got, filled, pct := r.pvLine(tt.args.cur, tt.args.total, time.Now())
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
		})
	}
}

func Test_ciRenderer_lineWidth(t *testing.T) {
	tests := []struct {
		name     string
		barWidth int
		want     int
	}{
		{name: "zero falls back to default", barWidth: 0, want: ciBarWidth},
		{name: "negative falls back to default", barWidth: -1, want: ciBarWidth},
		{name: "positive passes through", barWidth: 40, want: 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0)
			r.barWidth = tt.barWidth
			if got := r.lineWidth(); got != tt.want {
				t.Errorf("ciRenderer.lineWidth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_ciRenderer_renderWaveLine(t *testing.T) {
	type args struct {
		advance bool
	}
	tests := []struct {
		name             string
		barWidth         int
		barPos           int
		args             args
		wantLen          int
		wantStateChanges bool
	}{
		{name: "advance mutates tick and barPos", barPos: 0,
			args: args{advance: true}, wantLen: ciBarWidth, wantStateChanges: true},
		{name: "no advance leaves state intact", barPos: 0,
			args: args{advance: false}, wantLen: ciBarWidth, wantStateChanges: false},
		{name: "barPos at right edge does not panic", barPos: ciBarWidth - ciBarMarkerLen - 1,
			args: args{advance: false}, wantLen: ciBarWidth, wantStateChanges: false},
		{name: "barWidth 2 does not panic on division by zero", barWidth: 2, barPos: 0,
			args: args{advance: false}, wantLen: 2, wantStateChanges: false},
		{name: "barWidth 1 does not panic on division by zero", barWidth: 1, barPos: 0,
			args: args{advance: false}, wantLen: 1, wantStateChanges: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0)
			if tt.barWidth > 0 {
				r.barWidth = tt.barWidth
			}
			r.barPos = tt.barPos
			tickBefore, posBefore := r.tick, r.barPos
			got := r.renderWaveLine(tt.args.advance)
			if len(got) != tt.wantLen {
				t.Errorf("ciRenderer.renderWaveLine() len = %d, want %d",
					len(got), tt.wantLen)
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

func Test_ciRenderer_renderWaveLineWithBytes(t *testing.T) {
	type args struct {
		cur int64
	}
	tests := []struct {
		name         string
		args         args
		wantLen      int
		wantContains string
	}{
		{name: "bytes suffix right aligned", args: args{cur: 500},
			wantLen: ciBarWidth, wantContains: "500 B"},
		{name: "large value uses binary suffix", args: args{cur: 1 << 30},
			wantLen: ciBarWidth, wantContains: "GiB"},
		{name: "narrow bar shorter than suffix does not panic", args: args{cur: 500},
			wantLen: 3, wantContains: ""},
		{name: "bar exactly suffix width does not panic", args: args{cur: 500},
			wantLen: 5, wantContains: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCIRenderer(io.Discard, 0)
			r.startedAt = time.Now()
			if tt.wantLen != ciBarWidth {
				r.barWidth = tt.wantLen
			}
			got := r.renderWaveLineWithBytes(tt.args.cur)
			if len(got) != tt.wantLen {
				t.Errorf("ciRenderer.renderWaveLineWithBytes() len = %d, want %d",
					len(got), tt.wantLen)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("ciRenderer.renderWaveLineWithBytes() = %q, want substring %q",
					got, tt.wantContains)
			}
		})
	}
}

// TestRetryClosesIdleConnectionsRedials verifies that a request which hangs
// past ResponseHeaderTimeout does not pin subsequent retries to the same dead
// HTTP/2 connection: each attempt must open a fresh TCP connection.
func TestRetryClosesIdleConnectionsRedials(t *testing.T) {
	var newConns atomic.Int64
	var reqs atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if reqs.Add(1) <= 2 {
				time.Sleep(400 * time.Millisecond)
			}
			_, _ = w.Write([]byte("ok"))
		}),
	)
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	client := srv.Client()
	tr := client.Transport.(*http.Transport)
	tr.ResponseHeaderTimeout = 100 * time.Millisecond
	tr.DisableKeepAlives = true

	var attempts int
	err := Retry(context.Background(), RetryOptions{
		Name:   "redial",
		Stderr: io.Discard,
		BackoffOptions: BackoffOptions{
			MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
			BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
		},
		IdleCloser: tr,
	}, func(ctx context.Context) error {
		attempts++
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if int64(attempts) != newConns.Load() {
		t.Errorf("new connections = %d, want %d (one per attempt)",
			newConns.Load(), attempts)
	}
}

// rangeResumeHandler serves full a payload and honors Range requests. The
// first GET (no Range) writes only the first half and then aborts the
// connection; later Range requests return 206 with the requested remainder.
func rangeResumeHandler(
	t *testing.T, payload []byte, ranges *[]string,
) http.HandlerFunc {
	t.Helper()
	half := len(payload) / 2
	return func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		*ranges = append(*ranges, rng)
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				_, _ = w.Write(payload[:half])
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		var start int64
		_, _ = fmt.Sscanf(rng, "bytes=%d-", &start)
		rest := payload[start:]
		w.Header().Set("Content-Range", fmt.Sprintf(
			"bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(rest)
	}
}

func Test_downloadResumesFromPartial(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 512)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	var ranges []string
	srv := httptest.NewServer(rangeResumeHandler(t, payload, &ranges))
	defer srv.Close()

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "resume.bin")
	opts := &DownloadOptions{
		Name:          "resume.bin",
		FileMode:      0o644,
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
		WantDigest:    digest,
		Stderr:        io.Discard,
		BackoffOptions: BackoffOptions{
			MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
			BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
		},
	}
	if err := downloadWithRetry(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadWithRetry: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if len(ranges) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(ranges))
	}
	want := fmt.Sprintf("bytes=%d-", len(payload)/2)
	if ranges[1] != want {
		t.Errorf("second request Range = %q, want %q", ranges[1], want)
	}
	if _, err := os.Stat(partPath(opts, dest)); !os.IsNotExist(err) {
		t.Errorf("partial file must be gone after success, stat err = %v", err)
	}
}

// Test_downloadResetsPartialOn200 covers a server that ignores Range and
// replies 200 with the whole body: a pre-seeded stale partial file must be
// truncated so the result is not a corrupt concatenation.
func Test_downloadResetsPartialOn200(t *testing.T) {
	payload := []byte("the full and correct body")
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "reset.bin")
	opts := &DownloadOptions{
		Name:           "reset.bin",
		FileMode:       0o644,
		HTTPClient:     srv.Client(),
		NoProgressBar:  true,
		StallTimeout:   -1,
		Stderr:         io.Discard,
		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}
	// Seed a stale partial file that the ignored Range request would otherwise
	// prepend to the full body.
	part := partPath(opts, dest)
	if err := os.WriteFile(part, []byte("STALE-PREFIX"), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	if err := callDownloadOnce(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
}

// Test_downloadNoResumeSingleAttempt confirms the non-resume path is unchanged:
// a file that downloads in one attempt with no pre-existing partial ends up
// byte-identical and leaves no partial behind.
func Test_downloadNoResumeSingleAttempt(t *testing.T) {
	payload := []byte("one shot payload")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "oneshot.bin")
	opts := &DownloadOptions{
		Name:           "oneshot.bin",
		FileMode:       0o644,
		HTTPClient:     srv.Client(),
		NoProgressBar:  true,
		StallTimeout:   -1,
		WantDigest:     digest,
		Stderr:         io.Discard,
		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}
	if err := callDownloadOnce(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch")
	}
	if _, err := os.Stat(partPath(opts, dest)); !os.IsNotExist(err) {
		t.Errorf("no partial file expected, stat err = %v", err)
	}
}

// Test_downloadDropsPartialOn416 covers a stale partial that is already at
// least as long as the target: the server answers 416 and downloadOnce must
// drop the partial so a later attempt can restart clean.
func Test_downloadDropsPartialOn416(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = w.Write([]byte("body"))
		}),
	)
	defer srv.Close()

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "big.bin")
	opts := &DownloadOptions{
		Name:           "big.bin",
		FileMode:       0o644,
		HTTPClient:     srv.Client(),
		NoProgressBar:  true,
		StallTimeout:   -1,
		Stderr:         io.Discard,
		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}
	part := partPath(opts, dest)
	if err := os.WriteFile(part, []byte("already-long-enough"), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil || !strings.Contains(err.Error(), "416") {
		t.Fatalf("expected 416 error, got %v", err)
	}
	if _, statErr := os.Stat(part); !os.IsNotExist(statErr) {
		t.Errorf("partial file must be dropped on 416, stat err = %v", statErr)
	}
}

func Test_partPath(t *testing.T) {
	tests := []struct {
		name string
		opts *DownloadOptions
		dest string
		want string
	}{
		{
			name: "uses Name when set",
			opts: &DownloadOptions{Name: "tool"},
			dest: "/tmp/dir/tool.bin",
			want: filepath.Join("/tmp/dir", ".tool.part"),
		},
		{
			name: "falls back to dest base when Name empty",
			opts: &DownloadOptions{},
			dest: "/tmp/dir/asset.bin",
			want: filepath.Join("/tmp/dir", ".asset.bin.part"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := partPath(tt.opts, tt.dest); got != tt.want {
				t.Errorf("partPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_resolveTotal(t *testing.T) {
	resp := func(length int64, contentRange string) *http.Response {
		h := http.Header{}
		if contentRange != "" {
			h.Set("Content-Range", contentRange)
		}
		return &http.Response{ContentLength: length, Header: h}
	}
	tests := []struct {
		name     string
		resp     *http.Response
		existing int64
		resume   bool
		want     int64
	}{
		{"no resume returns content length", resp(50, ""), 0, false, 50},
		{"resume prefers content-range total", resp(30, "bytes 20-49/50"), 20,
			true, 50},
		{"resume adds existing when no range", resp(30, ""), 20, true, 50},
		{"resume unknown length returns as-is", resp(-1, ""), 20, true, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTotal(tt.resp, tt.existing, tt.resume)
			if got != tt.want {
				t.Errorf("resolveTotal() = %d, want %d", got, tt.want)
			}
		})
	}
}

func Test_parseContentRangeTotal(t *testing.T) {
	tests := []struct {
		header string
		want   int64
	}{
		{"bytes 0-9/100", 100},
		{"bytes 0-9/*", 0},
		{"malformed-without-slash", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			if got := parseContentRangeTotal(tt.header); got != tt.want {
				t.Errorf("parseContentRangeTotal(%q) = %d, want %d",
					tt.header, got, tt.want)
			}
		})
	}
}

func Test_transportIdleCloser(t *testing.T) {
	t.Run("nil client returns nil", func(t *testing.T) {
		if transportIdleCloser(nil) != nil {
			t.Error("expected nil for nil client")
		}
	})
	t.Run("http.Transport is returned", func(t *testing.T) {
		c := &http.Client{Transport: &http.Transport{}}
		if transportIdleCloser(c) == nil {
			t.Error("expected non-nil for *http.Transport")
		}
	})
	t.Run("non-closeIdler transport returns nil", func(t *testing.T) {
		c := &http.Client{Transport: stubTransport{}}
		if transportIdleCloser(c) != nil {
			t.Error("expected nil for transport without CloseIdleConnections")
		}
	})
}

// Test_downloadResumeSeedsCIRenderer resumes a download with the CI progress
// renderer enabled so the branch that pre-seeds the renderer with the already
// downloaded byte count is exercised.
func Test_downloadResumeSeedsCIRenderer(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 1024)
	var ranges []string
	srv := httptest.NewServer(rangeResumeHandler(t, payload, &ranges))
	defer srv.Close()

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "ciresume.bin")
	trueVal := true
	opts := &DownloadOptions{
		Name:         "ciresume.bin",
		FileMode:     0o644,
		HTTPClient:   srv.Client(),
		CIMode:       &trueVal,
		Stderr:       io.Discard,
		Stdout:       io.Discard,
		StallTimeout: -1,
		BackoffOptions: BackoffOptions{
			MaxRetriesTime: time.Second, BackoffInitial: time.Millisecond,
			BackoffStep: time.Millisecond, BackoffCap: time.Millisecond,
		},
	}
	if err := downloadWithRetry(context.Background(), opts, srv.URL, dest); err != nil {
		t.Fatalf("downloadWithRetry: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// Test_downloadOnceDigestFileError covers the branch where sha256File fails
// after a successful transfer: the finished partial file is unlinked at EOF
// (its fd stays open so Sync/Close succeed) so the by-path hash read returns
// ENOENT.
func Test_downloadOnceDigestFileError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("temp-file removal trick is linux-specific")
	}
	name := "digesterr.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, name)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	opts := &DownloadOptions{
		Name:           name,
		FileMode:       0o644,
		NoProgressBar:  true,
		StallTimeout:   -1,
		WantDigest:     strings.Repeat("a", 64),
		HTTPClient:     tmpUnlinkingClient(srv.Client(), name, destDir),
		BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	}
	err := callDownloadOnce(context.Background(), opts, srv.URL, dest)
	if err == nil {
		t.Fatalf("expected sha256File error, got nil")
	}
}
