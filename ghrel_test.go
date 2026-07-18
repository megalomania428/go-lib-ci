// cspell:ignore rawhex stallsig syncfail badurl copyerr defaultdest isdir chmodfail
// cspell:ignore ctxmid cimode nofile nontty pvline waveline humanb fmtdur speedewma
// cspell:ignore EWMA closefail slashpayload
package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParseDigest(t *testing.T) {
	if got := parseDigest("sha256:abc"); got != "abc" {
		t.Errorf("parseDigest returned %q", got)
	}
	if got := parseDigest("md5:abc"); got != "" { // DevSkim: ignore DS126858
		t.Errorf("non-sha256 digest must be dropped, got %q", got)
	}
	if got := parseDigest(""); got != "" {
		t.Errorf("empty digest must remain empty, got %q", got)
	}
}

func TestFetchGitHubReleaseHappyPath(t *testing.T) {
	payload := []byte("hello world payload")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	assetName := "ansible-11-001.AppImage"
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest",
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer secret" {
				t.Errorf("missing/invalid auth header: %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name":   assetName,
					"url":    assetURL,
					"digest": digest,
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "application/octet-stream" {
				t.Errorf("bad Accept: %q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer secret" {
				t.Errorf("bad auth on download: %q", got)
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			_, _ = w.Write(payload)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	dest := t.TempDir()
	path, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "owner/repo",
		AssetName:     assetName,
		DestDir:       dest,
		Token:         "secret",
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  -1,
	})
	if err != nil {
		t.Fatalf("FetchGitHubRelease: %v", err)
	}
	if path != filepath.Join(dest, assetName) {
		t.Errorf("unexpected dest path: %s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("unexpected mode: %v", info.Mode())
	}
}

func TestFetchGitHubReleaseTrailingSlash(t *testing.T) {
	assetName := "slash-test.bin"
	payload := []byte("slashpayload")
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": assetName,
					"url":  assetURL,
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			_, _ = w.Write(payload)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	dest := t.TempDir()
	_, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "owner/repo",
		AssetName:     assetName,
		DestDir:       dest,
		APIBaseURL:    srv.URL + "/",
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  -1,
		HTTPClient:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("trailing slash in APIBaseURL caused error: %v", err)
	}
}

func TestFetchGitHubReleaseDigestMismatch(t *testing.T) {
	payload := []byte("real payload")
	assetName := "asset.bin"
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/tags/v2",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2",
				"assets": []map[string]any{{
					"name":   assetName,
					"url":    assetURL,
					"digest": "sha256:" + strings.Repeat("0", 64),
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	_, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "owner/repo",
		Tag:           "v2",
		AssetName:     assetName,
		DestDir:       t.TempDir(),
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  -1,
	})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

func TestFetchGitHubReleaseFallback(t *testing.T) {
	dest := t.TempDir()
	existing := filepath.Join(dest, "asset.bin")
	if err := os.WriteFile(existing, []byte("cached"), 0o755); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/",
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	path, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:               "owner/repo",
		AssetName:          "asset.bin",
		DestDir:            dest,
		APIBaseURL:         srv.URL,
		NoProgressBar:      true,
		RetryMax:           2,
		RetrySleepMax:      10 * time.Millisecond,
		StallTimeout:       -1,
		FallbackToExisting: true,
	})
	if err != nil {
		t.Fatalf("fallback path must not error, got %v", err)
	}
	if path != existing {
		t.Errorf("expected fallback path %s, got %s", existing, path)
	}
}

func TestFetchGitHubReleaseStallWatchdog(t *testing.T) {
	assetName := "slow.bin"
	release := make(chan struct{})
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": assetName,
					"url":  assetURL,
				}},
			})
		})
	mux.HandleFunc("/slow",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte{'x'})
				f.Flush()
			}
			<-release
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/slow"
	defer close(release)
	_, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "owner/repo",
		AssetName:     assetName,
		DestDir:       t.TempDir(),
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  200 * time.Millisecond,
		StallLimit:    1024,
	})
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stalled error, got %v", err)
	}
}

func TestFetchGitHubRelease(t *testing.T) {
	dir := t.TempDir()
	blockerPath := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blockerPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	type args struct {
		ctx  context.Context
		opts FetchOptions
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name: "empty repo rejected",
			args: args{
				ctx: context.Background(),
				opts: FetchOptions{
					AssetName: "x.bin",
					DestDir:   dir,
				},
			},
			wantErr: true,
			errSub:  "repo required",
		},
		{
			name: "repo without slash rejected",
			args: args{
				ctx: context.Background(),
				opts: FetchOptions{
					Repo:      "owner",
					AssetName: "x.bin",
					DestDir:   dir,
				},
			},
			wantErr: true,
			errSub:  "owner/name",
		},
		{
			name: "empty asset name rejected",
			args: args{
				ctx: context.Background(),
				opts: FetchOptions{
					Repo:    "owner/repo",
					DestDir: dir,
				},
			},
			wantErr: true,
			errSub:  "AssetName required",
		},
		{
			name: "mkdir failure on dest under a file",
			args: args{
				ctx: context.Background(),
				opts: FetchOptions{
					Repo:      "owner/repo",
					AssetName: "x.bin",
					DestDir:   filepath.Join(blockerPath, "sub"),
				},
			},
			wantErr: true,
			errSub:  "mkdir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FetchGitHubRelease(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("FetchGitHubRelease() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("FetchGitHubRelease() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func TestFetchGitHubReleaseSkipDownload(t *testing.T) {
	payload := []byte("already there")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	assetName := "cached.bin"
	dest := t.TempDir()
	existing := filepath.Join(dest, assetName)
	if err := os.WriteFile(existing, payload, 0o755); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name":   assetName,
					"url":    assetURL,
					"digest": digest,
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Errorf("download must be skipped when local digest matches")
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	var stdout, stderr bytes.Buffer
	path, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		DestDir:       dest,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  -1,
		Stdout:        &stdout,
		Stderr:        &stderr,
	})
	if err != nil {
		t.Fatalf("FetchGitHubRelease: %v", err)
	}
	if path != existing {
		t.Errorf("expected %s, got %s", existing, path)
	}
	if !strings.Contains(stdout.String(), "up to date") {
		t.Errorf("expected skip log, got %q", stdout.String())
	}
}

func TestFetchGitHubReleaseDownloadFallback(t *testing.T) {
	dest := t.TempDir()
	existing := filepath.Join(dest, "asset.bin")
	if err := os.WriteFile(existing, []byte("cached"), 0o755); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": "asset.bin",
					"url":  assetURL,
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	var stderr bytes.Buffer
	path, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:               "o/r",
		AssetName:          "asset.bin",
		DestDir:            dest,
		APIBaseURL:         srv.URL,
		NoProgressBar:      true,
		RetryMax:           2,
		RetrySleepMax:      5 * time.Millisecond,
		StallTimeout:       -1,
		FallbackToExisting: true,
		Stderr:             &stderr,
	})
	if err != nil {
		t.Fatalf("expected fallback to existing, got %v", err)
	}
	if path != existing {
		t.Errorf("expected %s, got %s", existing, path)
	}
	if !strings.Contains(stderr.String(), "download failed, using existing binary") {
		t.Errorf("expected fallback log, got %q", stderr.String())
	}
}

func TestFetchGitHubReleaseFallbackNoExisting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/",
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:               "o/r",
		AssetName:          "asset.bin",
		DestDir:            t.TempDir(),
		APIBaseURL:         srv.URL,
		NoProgressBar:      true,
		RetryMax:           1,
		StallTimeout:       -1,
		FallbackToExisting: true,
	})
	if err == nil {
		t.Fatalf("expected error when fallback target does not exist")
	}
}

func TestFetchGitHubReleaseDefaultDestDir(t *testing.T) {
	payload := []byte("body")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	assetName := "defaultdest.bin"
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmpWD := t.TempDir()
	defer func() {
		_ = os.Chdir(wd)
	}()
	if err := os.Chdir(tmpWD); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name":   assetName,
					"url":    assetURL,
					"digest": digest,
				}},
			})
		})
	mux.HandleFunc("/asset",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	assetURL = srv.URL + "/asset"
	path, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		RetryMax:      1,
		StallTimeout:  -1,
	})
	if err != nil {
		t.Fatalf("FetchGitHubRelease: %v", err)
	}
	want := filepath.Join(tmpWD, assetName)
	if path != want {
		t.Errorf("expected default dest %s, got %s", want, path)
	}
}

// TestFetchGitHubReleaseGetwdError covers the os.Getwd failure branch: chdir
// into a temporary directory, remove it, then call FetchGitHubRelease with an
// empty DestDir so the working directory has to be resolved.
func TestFetchGitHubReleaseGetwdError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getwd-via-deleted-cwd trick is linux-specific")
	}
	tmp, err := os.MkdirTemp("", "getwd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
		_ = os.RemoveAll(tmp)
	})
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir into tmp: %v", err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatalf("remove tmp: %v", err)
	}
	// Working directory inode is now orphaned; os.Getwd() returns ENOENT.
	_, err = FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:      "owner/repo",
		AssetName: "asset.bin",
	})
	if err == nil || !strings.Contains(err.Error(), "resolve working directory") {
		t.Fatalf("expected resolve working directory error, got %v", err)
	}
}

// closeTmpFdViaProc scans /proc/self/fd for an open descriptor whose target
// contains assetName and closes it. Used on Linux to inject EBADF into Sync
// or Close on the temp file held by downloadOnce.
func closeTmpFdViaProc(assetName string) bool {
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
		if strings.Contains(link, assetName) {
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
// call closes any open fd whose path contains assetName and returns EOF.
type eofClosesReader struct {
	data      []byte
	pos       int
	closed    bool
	assetName string
}

func (r *eofClosesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		if !r.closed {
			r.closed = true
			closeTmpFdViaProc(r.assetName)
		}
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func Test_downloadOnceSyncError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd shutdown via /proc/self/fd is linux-only")
	}
	assetName := "syncfail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, assetName)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		StallTimeout:  -1,
		RetryMax:      1,
		// Wrap the response body so that after the body has been fully read,
		// the temp file's fd is closed via /proc/self/fd, forcing tmp.Sync to
		// return EBADF.
		HTTPClient: fdRewritingClient(srv.Client(), assetName),
	}
	asset := &ghAsset{Name: assetName, URL: srv.URL}

	err := downloadOnce(context.Background(), opts, asset, dest, "")
	if err == nil {
		t.Fatalf("expected Sync error, got nil")
	}
}

func Test_downloadOnceCloseError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd shutdown via /proc/self/fd is linux-only")
	}
	assetName := "closefail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, assetName)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	// Override osTempFile so that after Sync succeeds the fd is closed via
	// /proc/self/fd, causing the subsequent tmp.Close() to return EBADF.
	orig := osTempFile
	t.Cleanup(func() { osTempFile = orig })
	osTempFile = func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &closeErrFile{File: f}, nil
	}

	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		StallTimeout:  -1,
		RetryMax:      1,
		HTTPClient:    srv.Client(),
	}
	asset := &ghAsset{Name: assetName, URL: srv.URL}

	err := downloadOnce(context.Background(), opts, asset, dest, "")
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
func fdRewritingClient(base *http.Client, assetName string) *http.Client {
	c := *base
	c.Transport = &fdClosingTransport{base: base.Transport, assetName: assetName}
	return &c
}

type fdClosingTransport struct {
	base      http.RoundTripper
	assetName string
}

func (t *fdClosingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	orig := resp.Body
	resp.Body = &wrappedBody{r: &eofClosesReader{data: readAllClose(orig),
		assetName: t.assetName}}
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
// read, trigger removal of any temp files matching ".<assetName>.*" inside
// dirToClean. The temp file's fd stays open so Sync/Close succeed, but the
// subsequent os.Chmod on tmpPath fails with ENOENT.
func tmpUnlinkingClient(base *http.Client, assetName, dirToClean string) *http.Client {
	c := *base
	c.Transport = &tmpUnlinkTransport{
		base: base.Transport, assetName: assetName, dir: dirToClean,
	}
	return &c
}

type tmpUnlinkTransport struct {
	base      http.RoundTripper
	assetName string
	dir       string
}

func (t *tmpUnlinkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	orig := resp.Body
	resp.Body = &wrappedBody{r: &eofUnlinksReader{
		data: readAllClose(orig), dir: t.dir, assetName: t.assetName,
	}}
	return resp, nil
}

type eofUnlinksReader struct {
	data      []byte
	pos       int
	dir       string
	assetName string
	unlinked  bool
}

func (r *eofUnlinksReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		if !r.unlinked {
			r.unlinked = true
			removeTempFiles(r.dir, r.assetName)
		}
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func removeTempFiles(dir, assetName string) {
	prefix := "." + assetName + "."
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
	assetName := "chmodfail.bin"
	destDir := t.TempDir()
	dest := filepath.Join(destDir, assetName)
	payload := []byte("payload")

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}),
	)
	defer srv.Close()

	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,
		StallTimeout:  -1,
		RetryMax:      1,
		HTTPClient:    tmpUnlinkingClient(srv.Client(), assetName, destDir),
	}
	asset := &ghAsset{Name: assetName, URL: srv.URL}
	err := downloadOnce(context.Background(), opts, asset, dest, "")
	if err == nil {
		t.Fatalf("expected chmod error, got nil")
	}
}

func Test_fetchAssetWithRetry(t *testing.T) {
	goodMux := http.NewServeMux()
	goodMux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name":   "asset.bin",
					"url":    "http://example/asset", // DevSkim: ignore DS137138
					"digest": "sha256:abc",
				}},
			})
		})
	goodSrv := httptest.NewServer(goodMux)
	defer goodSrv.Close()

	badMux := http.NewServeMux()
	badMux.HandleFunc("/",
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
	badSrv := httptest.NewServer(badMux)
	defer badSrv.Close()

	type args struct {
		ctx  context.Context
		opts *FetchOptions
	}
	tests := []struct {
		name    string
		args    args
		want    *ghAsset
		wantErr bool
	}{
		{
			name: "returns asset metadata from latest",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: goodSrv.URL,
					HTTPClient: goodSrv.Client(),
					RetryMax:   2,
				},
			},
			want: &ghAsset{
				Name:   "asset.bin",
				URL:    "http://example/asset", // DevSkim: ignore DS137138
				Digest: "sha256:abc",
			},
			wantErr: false,
		},
		{
			name: "wraps non-200 with retry exhausted",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "asset.bin",
					APIBaseURL:    badSrv.URL,
					HTTPClient:    badSrv.Client(),
					RetryMax:      2,
					RetrySleepMax: time.Millisecond,
					Stderr:        io.Discard,
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fetchAssetWithRetry(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("fetchAssetWithRetry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("fetchAssetWithRetry() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func Test_setGitHubHeaders(t *testing.T) {
	type args struct {
		accept string
		token  string
	}
	tests := []struct {
		name        string
		args        args
		wantAuth    string
		wantAccept  string
		wantVersion string
	}{
		{
			name:        "with token sets bearer auth",
			args:        args{accept: githubAcceptV3, token: "abc"},
			wantAuth:    "Bearer abc",
			wantAccept:  githubAcceptV3,
			wantVersion: githubAPIVersion,
		},
		{
			name:        "without token omits auth header",
			args:        args{accept: "application/octet-stream", token: ""},
			wantAuth:    "",
			wantAccept:  "application/octet-stream",
			wantVersion: githubAPIVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"http://example", // DevSkim: ignore DS137138
				nil)
			setGitHubHeaders(req, tt.args.accept, tt.args.token)
			if got := req.Header.Get("Authorization"); got != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tt.wantAuth)
			}
			if got := req.Header.Get("Accept"); got != tt.wantAccept {
				t.Errorf("Accept = %q, want %q", got, tt.wantAccept)
			}
			if got := req.Header.Get("X-GitHub-Api-Version"); got != tt.wantVersion {
				t.Errorf("X-GitHub-Api-Version = %q, want %q", got, tt.wantVersion)
			}
		})
	}
}

func Test_fetchAsset(t *testing.T) {
	latestMux := http.NewServeMux()
	latestMux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name":   "asset.bin",
					"url":    "http://example/asset", // DevSkim: ignore DS137138
					"digest": "sha256:abc",
				}},
			})
		})
	latestSrv := httptest.NewServer(latestMux)
	defer latestSrv.Close()

	tagMux := http.NewServeMux()
	tagMux.HandleFunc("/repos/o/r/releases/tags/v9",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9",
				"assets": []map[string]any{{
					"name": "asset.bin",
					"url":  "http://example/asset", // DevSkim: ignore DS137138
				}},
			})
		})
	tagSrv := httptest.NewServer(tagMux)
	defer tagSrv.Close()

	notFoundMux := http.NewServeMux()
	notFoundMux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": "other.bin",
					"url":  "http://example/asset", // DevSkim: ignore DS137138
				}},
			})
		})
	notFoundSrv := httptest.NewServer(notFoundMux)
	defer notFoundSrv.Close()

	nonOkMux := http.NewServeMux()
	nonOkMux.HandleFunc("/",
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		})
	nonOkSrv := httptest.NewServer(nonOkMux)
	defer nonOkSrv.Close()

	badJSONMux := http.NewServeMux()
	badJSONMux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		})
	badJSONSrv := httptest.NewServer(badJSONMux)
	defer badJSONSrv.Close()

	type args struct {
		ctx  context.Context
		opts *FetchOptions
	}
	tests := []struct {
		name    string
		args    args
		want    *ghAsset
		wantErr bool
		errSub  string
	}{
		{
			name: "fetches latest asset",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: latestSrv.URL,
					HTTPClient: latestSrv.Client(),
				},
			},
			want: &ghAsset{
				Name:   "asset.bin",
				URL:    "http://example/asset", // DevSkim: ignore DS137138
				Digest: "sha256:abc",
			},
			wantErr: false,
		},
		{
			name: "fetches tagged asset",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					Tag:        "v9",
					AssetName:  "asset.bin",
					APIBaseURL: tagSrv.URL,
					HTTPClient: tagSrv.Client(),
				},
			},
			want: &ghAsset{
				Name: "asset.bin", URL: "http://example/asset", // DevSkim: ignore DS137138
			},
			wantErr: false,
		},
		{
			name: "asset not in release errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: notFoundSrv.URL,
					HTTPClient: notFoundSrv.Client(),
				},
			},
			wantErr: true,
			errSub:  "not found in release",
		},
		{
			name: "non-200 status errors with body",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: nonOkSrv.URL,
					HTTPClient: nonOkSrv.Client(),
				},
			},
			wantErr: true,
			errSub:  "404 Not Found",
		},
		{
			name: "invalid json errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: badJSONSrv.URL,
					HTTPClient: badJSONSrv.Client(),
				},
			},
			wantErr: true,
			errSub:  "decode release payload",
		},
		{
			name: "bad url fails request",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: "ht!tp://invalid-url-with-spaces and stuff",
					HTTPClient: latestSrv.Client(),
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fetchAsset(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("fetchAsset() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("fetchAsset() error = %v, want substring %q", err, tt.errSub)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("fetchAsset() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func Test_fetchAssetTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{}"))
		}),
	)
	srv.Close() // closed server -> connection refused
	opts := &FetchOptions{
		Repo:       "o/r",
		AssetName:  "asset.bin",
		APIBaseURL: srv.URL,
		HTTPClient: srv.Client(),
	}
	_, err := fetchAsset(context.Background(), opts)
	if err == nil {
		t.Fatalf("expected transport error from closed server")
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
		ctx        context.Context
		opts       *FetchOptions
		asset      *ghAsset
		dest       string
		wantDigest string
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
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "ok.bin",
					APIBaseURL:    srv.URL,
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
					RetryMax:      1,
				},
				asset:      &ghAsset{Name: "ok.bin", URL: srv.URL},
				dest:       filepath.Join(destDir, "ok.bin"),
				wantDigest: digest,
			},
			wantErr: false,
		},
		{
			name: "empty asset url errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "bad.bin",
					NoProgressBar: true,
					StallTimeout:  -1,
					RetryMax:      1,
					Stderr:        io.Discard,
				},
				asset: &ghAsset{Name: "bad.bin", URL: ""},
				dest:  filepath.Join(destDir, "bad.bin"),
			},
			wantErr: true,
			errSub:  "empty asset URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := downloadWithRetry(
				tt.args.ctx, tt.args.opts, tt.args.asset,
				tt.args.dest, tt.args.wantDigest,
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
		ctx        context.Context
		opts       *FetchOptions
		asset      *ghAsset
		dest       string
		wantDigest string
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
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "ok.bin",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				asset:      &ghAsset{Name: "ok.bin", URL: srv.URL},
				dest:       filepath.Join(destDir, "ok.bin"),
				wantDigest: goodDigest,
			},
			wantErr: false,
		},
		{
			name: "non-200 status errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "forbidden.bin",
					HTTPClient:    nonOkSrv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				asset: &ghAsset{Name: "forbidden.bin", URL: nonOkSrv.URL},
				dest:  filepath.Join(destDir, "forbidden.bin"),
			},
			wantErr: true,
			errSub:  "403 Forbidden",
		},
		{
			name: "digest mismatch errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "mis.bin",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				asset:      &ghAsset{Name: "mis.bin", URL: srv.URL},
				dest:       filepath.Join(destDir, "mis.bin"),
				wantDigest: strings.Repeat("a", 64),
			},
			wantErr: true,
			errSub:  "digest mismatch",
		},
		{
			name: "create temp failure errors",
			args: args{
				ctx: context.Background(),
				opts: &FetchOptions{
					Repo:          "o/r",
					AssetName:     "blocker_under_file",
					HTTPClient:    srv.Client(),
					NoProgressBar: true,
					StallTimeout:  -1,
				},
				asset: &ghAsset{Name: "blocker_under_file", URL: srv.URL},
				dest:  filepath.Join(destDir, "blocker_under_file", "child"),
			},
			wantErr: true,
			errSub:  "create temp file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.args.opts != nil && tt.args.opts.AssetName == "blocker_under_file" {
				blocker := filepath.Join(destDir, "blocker_under_file")
				if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
					t.Fatalf("seed blocker: %v", err)
				}
			}
			err := downloadOnce(
				tt.args.ctx, tt.args.opts, tt.args.asset,
				tt.args.dest, tt.args.wantDigest,
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
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     "closed.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
	}
	asset := &ghAsset{Name: "closed.bin", URL: srv.URL}
	err := downloadOnce(
		context.Background(), opts, asset,
		filepath.Join(destDir, "closed.bin"), "",
	)
	if err == nil {
		t.Fatalf("expected transport error from closed server")
	}
}

func Test_downloadOnceBadURL(t *testing.T) {
	destDir := t.TempDir()
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     "badurl.bin",
		NoProgressBar: true,
		StallTimeout:  -1,
	}
	asset := &ghAsset{Name: "badurl.bin", URL: "ht!tp://invalid url with spaces"}
	err := downloadOnce(
		context.Background(), opts, asset,
		filepath.Join(destDir, "badurl.bin"), "",
	)
	if err == nil {
		t.Fatalf("expected new request error")
	}
}

func Test_downloadOnceCopyError(t *testing.T) {
	destDir := t.TempDir()
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     "copyerr.bin",
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
		HTTPClient: &http.Client{
			Transport: &failingBodyTransport{},
		},
	}
	asset := &ghAsset{Name: "copyerr.bin",
		URL: "http://example/asset"} // DevSkim: ignore DS137138
	err := downloadOnce(
		context.Background(), opts, asset,
		filepath.Join(destDir, "copyerr.bin"), "",
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
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     "isdir.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
	}
	asset := &ghAsset{Name: "isdir.bin", URL: srv.URL}
	err := downloadOnce(context.Background(), opts, asset, dest, "")
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
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     "ctxmid.bin",
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  -1,
		Stderr:        io.Discard,
	}
	asset := &ghAsset{Name: "ctxmid.bin", URL: srv.URL}
	err := downloadOnce(
		ctx, opts, asset, filepath.Join(destDir, "ctxmid.bin"), "",
	)
	if err == nil {
		t.Fatalf("expected ctx-canceled error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func Test_downloadOnceTransferStalledShortCircuit(t *testing.T) {
	assetName := "stallsig.bin"
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
	opts := &FetchOptions{
		Repo:          "o/r",
		AssetName:     assetName,
		HTTPClient:    srv.Client(),
		NoProgressBar: true,
		StallTimeout:  100 * time.Millisecond,
		StallLimit:    1024 * 1024,
		Stderr:        io.Discard,
	}
	asset := &ghAsset{Name: assetName, URL: srv.URL}
	err := downloadOnce(
		context.Background(), opts, asset,
		filepath.Join(destDir, assetName), "",
	)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stalled error, got %v", err)
	}
}

func Test_copyWithProgress(t *testing.T) {
	type args struct {
		opts  *FetchOptions
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
				opts:  &FetchOptions{NoProgressBar: true},
				src:   strings.NewReader("plain copy"),
				total: 10,
			},
			wantDst: "plain copy",
			wantErr: false,
		},
		{
			name: "progress bar known total copies bytes",
			args: args{
				opts: &FetchOptions{
					AssetName:     "p.bin",
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
				opts: &FetchOptions{
					AssetName:     "p.bin",
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
				opts:  &FetchOptions{NoProgressBar: true},
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
		opts   *FetchOptions
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
				opts: &FetchOptions{RetryMax: 3, RetrySleepMax: time.Millisecond,
					Stderr: io.Discard},
				action: "ok",
				fn:     func(context.Context) error { return nil },
			},
			wantErr: false,
		},
		{
			name: "succeeds after transient failures",
			args: args{
				ctxFn: context.Background,
				opts: &FetchOptions{
					RetryMax: 3, RetrySleepMax: time.Millisecond, Stderr: io.Discard,
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
			name: "exhausts retries and returns last error",
			args: args{
				ctxFn: context.Background,
				opts: &FetchOptions{RetryMax: 2, RetrySleepMax: time.Millisecond,
					Stderr: io.Discard},
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
				opts: &FetchOptions{RetryMax: 3, RetrySleepMax: time.Millisecond,
					Stderr: io.Discard},
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
				opts: &FetchOptions{
					RetryMax: 5, RetrySleepMax: time.Minute, Stderr: io.Discard,
				},
				action: "cancel-during-sleep",
				fn:     func(context.Context) error { return errors.New("again") },
			},
			wantErr: true,
			errSub:  context.Canceled.Error(),
		},
		{
			name: "RetrySleepMax below 1s caps initial sleep",
			args: args{
				ctxFn: context.Background,
				opts: &FetchOptions{
					RetryMax:      2,
					RetrySleepMax: time.Millisecond,
					Stderr:        io.Discard,
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

func Test_parseDigest(t *testing.T) {
	type args struct {
		raw string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "empty stays empty",
			args: args{raw: ""},
			want: "",
		},
		{
			name: "sha256 prefix stripped",
			args: args{raw: "sha256:" + strings.Repeat("a", 64)},
			want: strings.Repeat("a", 64),
		},
		{
			name: "non-sha256 prefix dropped",
			args: args{raw: "md5:abc"}, // DevSkim: ignore DS126858
			want: "",
		},
		{
			name: "no prefix dropped",
			args: args{raw: "rawhex"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDigest(tt.args.raw); got != tt.want {
				t.Errorf("parseDigest() = %v, want %v", got, tt.want)
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

func Test_tagLabel(t *testing.T) {
	type args struct {
		tag string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "empty tag maps to latest",
			args: args{tag: ""},
			want: "latest",
		},
		{
			name: "non-empty tag passes through",
			args: args{tag: "v1.2.3"},
			want: "v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tagLabel(tt.args.tag); got != tt.want {
				t.Errorf("tagLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_newHTTPClient(t *testing.T) {
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
			got := newHTTPClient(tt.connectTimeout)
			if got == nil {
				t.Fatalf("newHTTPClient() returned nil")
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
			if tr.IdleConnTimeout != 30*time.Second {
				t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, 30*time.Second)
			}
			if tr.DialContext == nil {
				t.Errorf("DialContext must be set")
			}
		})
	}
}

func Test_useCIProgress(t *testing.T) {
	trueVal, falseVal := true, false
	t.Run("CIMode forced true", func(t *testing.T) {
		if !useCIProgress(&FetchOptions{CIMode: &trueVal, Stderr: os.Stdout}) {
			t.Error("expected true when CIMode=true")
		}
	})
	t.Run("CIMode forced false", func(t *testing.T) {
		if useCIProgress(&FetchOptions{CIMode: &falseVal, Stderr: os.Stdout}) {
			t.Error("expected false when CIMode=false")
		}
	})
	t.Run("non-file Stderr is CI", func(t *testing.T) {
		if !useCIProgress(&FetchOptions{Stderr: &bytes.Buffer{}}) {
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
		if !useCIProgress(&FetchOptions{Stderr: f}) {
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
	opts := &FetchOptions{
		AssetName:  "out.bin",
		FileMode:   0o644,
		HTTPClient: srv.Client(),
		CIMode:     &trueVal,
		Stderr:     io.Discard,
		Stdout:     io.Discard,
	}
	asset := &ghAsset{Name: "out.bin", URL: srv.URL}
	if err := downloadOnce(context.Background(), opts, asset, dest, ""); err != nil {
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
	opts := &FetchOptions{
		AssetName:  "out.bin",
		FileMode:   0o644,
		HTTPClient: srv.Client(),
		CIMode:     &trueVal,
		Stderr:     io.Discard,
		Stdout:     io.Discard,
	}
	asset := &ghAsset{Name: "out.bin", URL: srv.URL}
	if err := downloadOnce(context.Background(), opts, asset, dest, ""); err != nil {
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
		opts  *FetchOptions
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
				opts: &FetchOptions{
					AssetName:     "p.bin",
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
				opts: &FetchOptions{
					AssetName:     "p.bin",
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
				opts: &FetchOptions{
					AssetName:     "p.bin",
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
			opts := &FetchOptions{NoProgressBar: false}
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
			if got.barMove != 1 || got.tick != 150 || got.lastPct != -1 {
				t.Errorf("default state mismatch: barMove=%d tick=%d lastPct=%v",
					got.barMove, got.tick, got.lastPct)
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
		lastPct   float64
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
			fields:      fields{lastBar: 0, lastPct: -1, lastPrint: time.Time{}},
			args:        args{cur: 500, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name:        "bar advance forces print within window",
			fields:      fields{lastBar: 0, lastPct: 0, lastPrint: now},
			args:        args{cur: 900, total: 1000, now: now},
			wantPrinted: true,
		},
		{
			name:        "stale entry older than one second prints",
			fields:      fields{lastBar: -1, lastPct: 50, lastPrint: now.Add(-2 * time.Second)},
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
			r.lastPct = tt.fields.lastPct
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
