// cspell:ignore rawhex defaultdest slashpayload
package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
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

		StallTimeout: -1, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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

		StallTimeout: -1,
		HTTPClient:   srv.Client(), BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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

		StallTimeout: -1, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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
		Repo:          "owner/repo",
		AssetName:     "asset.bin",
		DestDir:       dest,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,

		StallTimeout:       -1,
		FallbackToExisting: true, BackoffOptions: BackoffOptions{
			MaxRetriesTime: 20 * time.Millisecond,
			BackoffInitial: 10 * time.Millisecond,
			BackoffStep:    10 * time.Millisecond,
			BackoffCap:     10 * time.Millisecond,
		},
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

		StallTimeout: 200 * time.Millisecond,
		StallLimit:   1024, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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

		StallTimeout: -1,
		Stdout:       &stdout,
		Stderr:       &stderr, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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
		Repo:          "o/r",
		AssetName:     "asset.bin",
		DestDir:       dest,
		APIBaseURL:    srv.URL,
		NoProgressBar: true,

		StallTimeout:       -1,
		FallbackToExisting: true,
		Stderr:             &stderr, BackoffOptions: BackoffOptions{
			MaxRetriesTime: 10 * time.Millisecond,
			BackoffInitial: 5 * time.Millisecond,
			BackoffStep:    5 * time.Millisecond,
			BackoffCap:     5 * time.Millisecond,
		},
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
		Repo:          "o/r",
		AssetName:     "asset.bin",
		DestDir:       t.TempDir(),
		APIBaseURL:    srv.URL,
		NoProgressBar: true,

		StallTimeout:       -1,
		FallbackToExisting: true, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	})
	if err == nil {
		t.Fatalf("expected error when fallback target does not exist")
	}
}

// TestFetchGitHubReleaseEmptyAssetURL covers the branch in FetchGitHubRelease
// that rejects a release payload whose matched asset carries no download URL,
// once past the digest short-circuit.
func TestFetchGitHubReleaseEmptyAssetURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": "asset.bin",
					"url":  "",
				}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := FetchGitHubRelease(context.Background(), FetchOptions{
		Repo:          "o/r",
		AssetName:     "asset.bin",
		DestDir:       t.TempDir(),
		APIBaseURL:    srv.URL,
		NoProgressBar: true,

		StallTimeout: -1, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
	})
	if err == nil || !strings.Contains(err.Error(), "empty asset URL") {
		t.Fatalf("expected empty asset URL error, got %v", err)
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

		StallTimeout: -1, BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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
					HTTPClient: goodSrv.Client(), BackoffOptions: BackoffOptions{MaxRetriesTime: -1},
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
					Repo:       "o/r",
					AssetName:  "asset.bin",
					APIBaseURL: badSrv.URL,
					HTTPClient: badSrv.Client(),

					Stderr: io.Discard, BackoffOptions: BackoffOptions{MaxRetriesTime: time.Millisecond,
						BackoffInitial: time.Millisecond,
						BackoffStep:    time.Millisecond,
						BackoffCap:     time.Millisecond},
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
