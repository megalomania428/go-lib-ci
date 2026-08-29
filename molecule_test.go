// Package ci is documented in doc.go.
// cspell:ignore appimage roles2test dpkg GOTESTS gaiad moreutils
package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// writeFakeBin creates an executable shell script at dir/name that prints its
// arguments and the ANSIBLE_LOG_PATH / ANSIBLE_ROLES_PATH environment
// variables to stdout, then exits with the given code.
func writeFakeBin(t *testing.T, dir, name string, code int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\necho \"args: $*\"\n" +
		"echo \"ANSIBLE_LOG_PATH=$ANSIBLE_LOG_PATH\"\n" +
		"echo \"ANSIBLE_ROLES_PATH=$ANSIBLE_ROLES_PATH\"\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bin %s: %v", name, err)
	}
	return path
}

func TestPrepare(t *testing.T) {
	assetName := "ansible-11-001.AppImage"
	payload := []byte("fake appimage payload")

	t.Run("missing RoleDir errors", func(t *testing.T) {
		_, err := Prepare(context.Background(), PrepareOptions{})
		if err == nil {
			t.Error("expected error when RoleDir is empty")
		}
	})

	t.Run("missing github token errors when required", func(t *testing.T) {
		t.Setenv(GitHubTokenEnv, "")
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir: "role", RequireGitHubToken: true,
		})
		if err == nil {
			t.Error("expected error when GitHubTokenEnv is unset")
		}
	})

	t.Run("full happy path links role and downloads appimage", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		homeDir := t.TempDir()
		sourceDir := t.TempDir()
		rolesBase := t.TempDir()
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		got, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "ansible-mega-base",
			SourceDir:     sourceDir,
			AppImageName:  assetName,
			Isolated:      true,
			TmpBase:       rolesBase,
			TmpPrefix:     "mega-base",
			HomeDir:       homeDir,
			FetchOverride: fakeFetchOverride(srv),
			Stdout:        stdout,
			Stderr:        stderr,
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		wantAppImage := filepath.Join(homeDir, "bin", assetName)
		if got.AppImageBin != wantAppImage {
			t.Errorf("AppImageBin = %q, want %q", got.AppImageBin, wantAppImage)
		}
		if want := filepath.Join(got.IsolatedDir, "roles2test"); got.RolesPath != want {
			t.Errorf("RolesPath = %q, want %q", got.RolesPath, want)
		}
		resolved, err := os.Readlink(filepath.Join(got.RolesPath, "ansible-mega-base"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if resolved != sourceDir {
			t.Errorf("role symlink target = %q, want %q", resolved, sourceDir)
		}
	})

	t.Run("empty SourceDir uses working directory", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		homeDir := t.TempDir()
		wantWd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		got, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  assetName,
			Isolated:      true,
			TmpBase:       t.TempDir(),
			TmpPrefix:     "mega-base",
			HomeDir:       homeDir,
			FetchOverride: fakeFetchOverride(srv),
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		resolved, err := os.Readlink(filepath.Join(got.RolesPath, "role"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if resolved != wantWd {
			t.Errorf("role symlink target = %q, want %q", resolved, wantWd)
		}
	})

	t.Run("ensure moreutils failure propagates", func(t *testing.T) {
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir: "role", EnsureMoreutils: true,
			PackagesOverride: func(o *EnsurePackagesOptions) {
				o.DpkgStatusFile = filepath.Join(t.TempDir(), "does-not-exist")
			},
		})
		if err == nil {
			t.Error("expected error when dpkg status file is missing")
		}
	})

	t.Run("fetch appimage failure propagates", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  "missing.AppImage",
			HomeDir:       t.TempDir(),
			FetchOverride: fakeFetchOverride(srv),
		})
		if err == nil {
			t.Error("expected error for missing asset")
		}
	})

	t.Run("resolve paths failure propagates", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  assetName,
			Isolated:      true,
			HomeDir:       t.TempDir(),
			FetchOverride: fakeFetchOverride(srv),
		})
		if err == nil {
			t.Error("expected error when Isolated is true without TmpPrefix")
		}
	})

	t.Run("link role failure propagates", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		blockedBase := t.TempDir()
		blocker := filepath.Join(blockedBase, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed blocker file: %v", err)
		}
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  assetName,
			Isolated:      true,
			TmpBase:       blocker,
			TmpPrefix:     "mega-base",
			HomeDir:       t.TempDir(),
			FetchOverride: fakeFetchOverride(srv),
		})
		if err == nil {
			t.Error("expected error when TmpBase parent is not a directory")
		}
	})

	t.Run("link role failure with resolved paths propagates", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		restore := uuidV7
		uuidV7 = func() (string, error) { return "fixed-uuid", nil }
		defer func() { uuidV7 = restore }()
		base := t.TempDir()
		isolated := filepath.Join(base, "mega-base-tests-fixed-uuid")
		if err := os.MkdirAll(isolated, 0o755); err != nil {
			t.Fatalf("seed isolated dir: %v", err)
		}
		if err := os.Chmod(isolated, 0o555); err != nil {
			t.Fatalf("chmod read-only: %v", err)
		}
		defer func() { _ = os.Chmod(isolated, 0o755) }()
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  assetName,
			Isolated:      true,
			TmpBase:       base,
			TmpPrefix:     "mega-base",
			HomeDir:       t.TempDir(),
			FetchOverride: fakeFetchOverride(srv),
		})
		if err == nil {
			t.Error("expected error when linkRole cannot create roles2test dir")
		}
	})

	t.Run("getwd failure propagates", func(t *testing.T) {
		srv := newFakeAppImageServer(t, assetName, payload)
		restore := osGetwdFn
		osGetwdFn = func() (string, error) { return "", errors.New("boom") }
		defer func() { osGetwdFn = restore }()
		_, err := Prepare(context.Background(), PrepareOptions{
			RoleDir:       "role",
			AppImageName:  assetName,
			HomeDir:       t.TempDir(),
			FetchOverride: fakeFetchOverride(srv),
		})
		if err == nil {
			t.Error("expected error when osGetwdFn fails")
		}
	})
}

// fakeFetchOverride returns a FetchOverride that points FetchOptions at srv
// and disables retries/progress/stall-timeout for fast, deterministic tests.
func fakeFetchOverride(srv *httptest.Server) func(*FetchOptions) {
	return func(o *FetchOptions) {
		o.APIBaseURL = srv.URL
		o.NoProgressBar = true
		o.MaxRetriesTime = -1
		o.StallTimeout = -1
		o.HTTPClient = srv.Client()
	}
}

// newFakeAppImageServer starts an httptest server serving a single release
// asset named assetName with the given payload, mimicking the GitHub
// releases API subset used by fetchAppImage.
func newFakeAppImageServer(
	t *testing.T,
	assetName string,
	payload []byte,
) *httptest.Server {
	t.Helper()
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/raven428/container-images/releases/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1",
				"assets": []map[string]any{{
					"name": assetName, "url": assetURL,
				}},
			})
		})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	assetURL = srv.URL + "/asset"
	return srv
}

func Test_fetchAppImage(t *testing.T) {
	assetName := "ansible-11-001.AppImage"
	payload := []byte("fake appimage payload")
	srv := newFakeAppImageServer(t, assetName, payload)

	t.Run("downloads asset via FetchGitHubRelease", func(t *testing.T) {
		dest := t.TempDir()
		t.Setenv("HOME", dest)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		got, err := fetchAppImage(context.Background(), PrepareOptions{
			AppImageName:  assetName,
			FetchOverride: fakeFetchOverride(srv),
		}, stdout, stderr)
		if err != nil {
			t.Fatalf("fetchAppImage() error = %v", err)
		}
		want := filepath.Join(dest, "bin", assetName)
		if got != want {
			t.Errorf("fetchAppImage() = %q, want %q", got, want)
		}
		gotPayload, err := os.ReadFile(got)
		if err != nil {
			t.Fatalf("read downloaded: %v", err)
		}
		if string(gotPayload) != string(payload) {
			t.Errorf("payload mismatch: %q", gotPayload)
		}
	})

	t.Run("empty options use default name and home", func(t *testing.T) {
		dest := t.TempDir()
		t.Setenv("HOME", dest)
		got, err := fetchAppImage(context.Background(), PrepareOptions{
			FetchOverride: func(o *FetchOptions) {
				o.APIBaseURL = srv.URL
				o.AssetName = assetName
				o.NoProgressBar = true
				o.MaxRetriesTime = -1
				o.StallTimeout = -1
				o.HTTPClient = srv.Client()
			},
		}, io.Discard, io.Discard)
		if err != nil {
			t.Fatalf("fetchAppImage() error = %v", err)
		}
		if want := filepath.Join(dest, "bin", assetName); got != want {
			t.Errorf("fetchAppImage() = %q, want %q", got, want)
		}
	})

	t.Run("failure without fallback propagates", func(t *testing.T) {
		_, err := fetchAppImage(context.Background(), PrepareOptions{
			AppImageName:    "missing.AppImage",
			AppImageRelease: "v-does-not-exist",
			HomeDir:         t.TempDir(),
			FetchOverride: func(o *FetchOptions) {
				o.APIBaseURL = srv.URL
				o.NoProgressBar = true
				o.MaxRetriesTime = -1
				o.StallTimeout = -1
				o.HTTPClient = srv.Client()
			},
		}, io.Discard, io.Discard)
		if err == nil {
			t.Error("expected error for missing release tag")
		}
	})

	t.Run("token env vars are forwarded as bearer token", func(t *testing.T) {
		var gotAuth string
		authMux := http.NewServeMux()
		authMux.HandleFunc("/repos/raven428/container-images/releases/latest",
			func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v1", "assets": []map[string]any{},
				})
			})
		authSrv := httptest.NewServer(authMux)
		t.Cleanup(authSrv.Close)
		t.Setenv(GitHubTokenEnv, "secret-token")
		homeDir := t.TempDir()
		_, err := fetchAppImage(context.Background(), PrepareOptions{
			AppImageName: "not-found.AppImage",
			HomeDir:      homeDir,
			FetchOverride: func(o *FetchOptions) {
				o.APIBaseURL = authSrv.URL
				o.NoProgressBar = true
				o.MaxRetriesTime = -1
				o.StallTimeout = -1
				o.HTTPClient = authSrv.Client()
			},
		}, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("expected error, asset does not exist on fake server")
		}
		if gotAuth != "Bearer secret-token" {
			t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-token")
		}
	})
}

func Test_resolveMoleculePaths(t *testing.T) {
	restoreUUID := uuidV7
	uuidV7 = func() (string, error) { return "fixed-uuid", nil }
	defer func() { uuidV7 = restoreUUID }()

	t.Run("non isolated reuses shared paths", func(t *testing.T) {
		rolesPath, logBase, isolatedDir, err := resolveMoleculePaths(PrepareOptions{})
		if err != nil {
			t.Fatalf("resolveMoleculePaths() error = %v", err)
		}
		if rolesPath != defaultRolesPath {
			t.Errorf("rolesPath = %q, want %q", rolesPath, defaultRolesPath)
		}
		if want := defaultLogBase + "-fixed-uuid"; logBase != want {
			t.Errorf("logBase = %q, want %q", logBase, want)
		}
		if isolatedDir != "" {
			t.Errorf("isolatedDir = %q, want empty", isolatedDir)
		}
	})

	t.Run("isolated without prefix errors", func(t *testing.T) {
		_, _, _, err := resolveMoleculePaths(PrepareOptions{Isolated: true})
		if err == nil {
			t.Error("expected error when TmpPrefix is empty")
		}
	})

	t.Run("isolated with explicit TmpBase creates unique dir", func(t *testing.T) {
		base := t.TempDir()
		rolesPath, logBase, isolatedDir, err := resolveMoleculePaths(PrepareOptions{
			Isolated: true, TmpBase: base, TmpPrefix: "mega-base",
		})
		if err != nil {
			t.Fatalf("resolveMoleculePaths() error = %v", err)
		}
		wantDir := filepath.Join(base, "mega-base-tests-fixed-uuid")
		if isolatedDir != wantDir {
			t.Errorf("isolatedDir = %q, want %q", isolatedDir, wantDir)
		}
		if want := filepath.Join(isolatedDir, "roles2test"); rolesPath != want {
			t.Errorf("rolesPath = %q, want %q", rolesPath, want)
		}
		wantLogBase := filepath.Join(isolatedDir, "molecule-fixed-uuid")
		if logBase != wantLogBase {
			t.Errorf("logBase = %q, want %q", logBase, wantLogBase)
		}
	})

	t.Run("isolated falls back to RUNNER_TEMP then default base", func(t *testing.T) {
		runnerTemp := t.TempDir()
		t.Setenv(runnerTempEnv, runnerTemp)
		_, _, isolatedDir, err := resolveMoleculePaths(PrepareOptions{
			Isolated: true, TmpPrefix: "mega-base",
		})
		if err != nil {
			t.Fatalf("resolveMoleculePaths() error = %v", err)
		}
		wantDir := filepath.Join(runnerTemp, "mega-base-tests-fixed-uuid")
		if isolatedDir != wantDir {
			t.Errorf("isolatedDir = %q, want %q", isolatedDir, wantDir)
		}
	})

	t.Run("isolated falls back to default base when unset", func(t *testing.T) {
		t.Setenv(runnerTempEnv, "")
		_, _, isolatedDir, err := resolveMoleculePaths(PrepareOptions{
			Isolated: true, TmpPrefix: "mega-base-default-base-test",
		})
		if err != nil {
			t.Fatalf("resolveMoleculePaths() error = %v", err)
		}
		defer func() { _ = os.RemoveAll(isolatedDir) }()
		wantDir := filepath.Join(defaultIsolatedBase,
			"mega-base-default-base-test-tests-fixed-uuid")
		if isolatedDir != wantDir {
			t.Errorf("isolatedDir = %q, want %q", isolatedDir, wantDir)
		}
	})

	t.Run("mkdir all failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed blocker file: %v", err)
		}
		_, _, _, err := resolveMoleculePaths(PrepareOptions{
			Isolated: true, TmpBase: blocker, TmpPrefix: "mega-base",
		})
		if err == nil {
			t.Error("expected error when isolated tmp parent is a file")
		}
	})

	t.Run("uuid failure propagates", func(t *testing.T) {
		restore := uuidV7
		uuidV7 = func() (string, error) { return "", errors.New("boom") }
		defer func() { uuidV7 = restore }()
		_, _, _, err := resolveMoleculePaths(PrepareOptions{})
		if err == nil {
			t.Error("expected error when uuidV7 fails")
		}
	})
}

func Test_cleanupIsolatedDir(t *testing.T) {
	t.Run("empty dir is a no-op", func(_ *testing.T) {
		cleanupIsolatedDir("")
	})
	t.Run("removes an existing directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "isolated")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		cleanupIsolatedDir(dir)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("cleanupIsolatedDir() left %q behind, stat err = %v", dir, err)
		}
	})
}

func Test_linkRole(t *testing.T) {
	t.Run("creates roles path and symlink", func(t *testing.T) {
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles2test")
		sourceDir := filepath.Join(dir, "source")
		if err := os.MkdirAll(sourceDir, 0o755); err != nil {
			t.Fatalf("seed source dir: %v", err)
		}
		stdout := &bytes.Buffer{}
		if err := linkRole(rolesPath, "my-role", sourceDir, stdout); err != nil {
			t.Fatalf("linkRole() error = %v", err)
		}
		target := filepath.Join(rolesPath, "my-role")
		resolved, err := os.Readlink(target)
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if resolved != sourceDir {
			t.Errorf("symlink target = %q, want %q", resolved, sourceDir)
		}
		wantStdout := "==> role linked [" + target + "] -> [" + sourceDir + "]\n"
		if got := stdout.String(); got != wantStdout {
			t.Errorf("linkRole() stdout = %q, want %q", got, wantStdout)
		}
	})
	t.Run("replaces a stale symlink", func(t *testing.T) {
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles2test")
		oldSource := filepath.Join(dir, "old")
		newSource := filepath.Join(dir, "new")
		for _, d := range []string{oldSource, newSource} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("seed %s: %v", d, err)
			}
		}
		if err := linkRole(rolesPath, "my-role", oldSource, &bytes.Buffer{}); err != nil {
			t.Fatalf("linkRole() first call error = %v", err)
		}
		if err := linkRole(rolesPath, "my-role", newSource, &bytes.Buffer{}); err != nil {
			t.Fatalf("linkRole() second call error = %v", err)
		}
		resolved, err := os.Readlink(filepath.Join(rolesPath, "my-role"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if resolved != newSource {
			t.Errorf("symlink target = %q, want %q", resolved, newSource)
		}
	})
	t.Run("mkdir failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed blocker file: %v", err)
		}
		err := linkRole(
			filepath.Join(blocker, "roles2test"), "my-role", dir, &bytes.Buffer{})
		if err == nil {
			t.Error("expected error when roles path parent is a file")
		}
	})
	t.Run("symlink failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles2test")
		if err := os.MkdirAll(rolesPath, 0o755); err != nil {
			t.Fatalf("seed roles path: %v", err)
		}
		if err := os.Chmod(rolesPath, 0o555); err != nil {
			t.Fatalf("chmod read-only: %v", err)
		}
		defer func() { _ = os.Chmod(rolesPath, 0o755) }()
		err := linkRole(rolesPath, "my-role", dir, &bytes.Buffer{})
		if err == nil {
			t.Error("expected error when roles path is read-only")
		}
	})
	t.Run("remove stale role failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles2test")
		if err := os.MkdirAll(rolesPath, 0o755); err != nil {
			t.Fatalf("seed roles path: %v", err)
		}
		stale := filepath.Join(rolesPath, "my-role")
		if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed stale role file: %v", err)
		}
		if err := os.Chmod(rolesPath, 0o555); err != nil {
			t.Fatalf("chmod read-only: %v", err)
		}
		defer func() { _ = os.Chmod(rolesPath, 0o755) }()
		err := linkRole(rolesPath, "my-role", dir, &bytes.Buffer{})
		if err == nil {
			t.Error("expected error when stale role cannot be removed")
		}
	})
}

func Test_firstNonEmpty(t *testing.T) {
	type args struct {
		values []string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{name: "no values", args: args{values: nil}, want: ""},
		{name: "all empty", args: args{values: []string{"", ""}}, want: ""},
		{name: "first non-empty wins", args: args{values: []string{"a", "b"}}, want: "a"},
		{
			name: "skips leading empties",
			args: args{values: []string{"", "", "b"}},
			want: "b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNonEmpty(tt.args.values...); got != tt.want {
				t.Errorf("firstNonEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMoleculeCreate(t *testing.T) {
	t.Setenv("ANSIBLE_ROLES_PATH", "")
	dir := t.TempDir()
	okBin := writeFakeBin(t, dir, "molecule-ok", 0)
	failBin := writeFakeBin(t, dir, "molecule-fail", 1)
	type args struct {
		ctx  context.Context
		opts MoleculeCreateOptions
	}
	tests := []struct {
		name        string
		args        args
		checkStdout bool
		wantStdout  string
		wantErr     bool
	}{
		{
			name:    "missing binary errors",
			args:    args{ctx: context.Background(), opts: MoleculeCreateOptions{}},
			wantErr: true,
		},
		{
			name: "success runs create with log suffix",
			args: args{ctx: context.Background(), opts: MoleculeCreateOptions{
				MoleculeBinary: okBin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			}},
			checkStdout: true,
			wantStdout: "\n\n\nmolecule [create] action\n" +
				"args: molecule -v create -s default\n" +
				"ANSIBLE_LOG_PATH=/tmp/molecule-x-0create\n" +
				"ANSIBLE_ROLES_PATH=\n",
		},
		{
			name: "default stdout and stderr are used when nil",
			args: args{ctx: context.Background(), opts: MoleculeCreateOptions{
				MoleculeBinary: okBin, Scenario: "default", LogBase: "/tmp/molecule-x",
			}},
		},
		{
			name: "propagates command failure",
			args: args{ctx: context.Background(), opts: MoleculeCreateOptions{
				MoleculeBinary: failBin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MoleculeCreate(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MoleculeCreate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr || !tt.checkStdout {
				return
			}
			got := tt.args.opts.Stdout.(*bytes.Buffer).String()
			if got != tt.wantStdout {
				t.Errorf("MoleculeCreate() stdout = %q, want %q", got, tt.wantStdout)
			}
		})
	}
}

// writeCountingFailBin creates a fake molecule binary that succeeds on its
// first failAfter-1 invocations (counted via a file in dir) and fails on the
// failAfter-th one, letting tests target one specific call in RunGroup's
// sequence of five molecule invocations.
func writeCountingFailBin(t *testing.T, dir string, failAfter int) string {
	t.Helper()
	path := filepath.Join(dir, "molecule-counting")
	counter := filepath.Join(dir, "counter")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	body := "#!/bin/sh\n" +
		"n=$(cat '" + counter + "')\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > '" + counter + "'\n" +
		"echo \"call $n: $*\"\n" +
		"[ \"$n\" -eq " + strconv.Itoa(failAfter) + " ] && exit 1\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write counting fail bin: %v", err)
	}
	return path
}

func TestRunGroup(t *testing.T) {
	dir := t.TempDir()
	okBin := writeFakeBin(t, dir, "molecule-ok", 0)
	type args struct {
		ctx  context.Context
		opts RunGroupOptions
	}
	tests := []struct {
		name        string
		args        args
		wantCounter int
		wantErr     bool
	}{
		{
			name:    "missing binary errors",
			args:    args{ctx: context.Background(), opts: RunGroupOptions{}},
			wantErr: true,
		},
		{
			name: "missing counter errors",
			args: args{ctx: context.Background(), opts: RunGroupOptions{
				MoleculeBinary: okBin,
			}},
			wantErr: true,
		},
		{
			name: "empty tag runs full sequence",
			args: args{ctx: context.Background(), opts: RunGroupOptions{
				MoleculeBinary: okBin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Counter: newIntPtr(1),
			}},
			wantCounter: 6,
		},
		{
			name: "tagged group runs full sequence",
			args: args{ctx: context.Background(), opts: RunGroupOptions{
				MoleculeBinary: okBin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Tag: "service-gaiad,service-start", Counter: newIntPtr(1),
			}},
			wantCounter: 6,
		},
		{
			name: "with roles path runs full sequence",
			args: args{ctx: context.Background(), opts: RunGroupOptions{
				MoleculeBinary: okBin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Counter: newIntPtr(1), RolesPath: "/tmp/some/roles",
			}},
			wantCounter: 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RunGroup(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunGroup() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := *tt.args.opts.Counter; got != tt.wantCounter {
				t.Errorf("RunGroup() counter = %d, want %d", got, tt.wantCounter)
			}
		})
	}
	t.Run("failure at each step propagates and stops the sequence", func(t *testing.T) {
		for step := 1; step <= 5; step++ {
			bin := writeCountingFailBin(t, t.TempDir(), step)
			counter := newIntPtr(1)
			err := RunGroup(context.Background(), RunGroupOptions{
				MoleculeBinary: bin, Scenario: "default", LogBase: "/tmp/molecule-x",
				Counter: counter,
			})
			if err == nil {
				t.Errorf("step %d: expected error, got nil", step)
			}
			if *counter != step {
				t.Errorf(
					"step %d: counter stopped at %d, want %d", step, *counter, step)
			}
		}
	})
}

// newIntPtr returns a pointer to a new int initialized to v.
func newIntPtr(v int) *int {
	return &v
}

func Test_tagPrefix(t *testing.T) {
	type args struct {
		tag string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{name: "empty tag", args: args{tag: ""}, want: ""},
		{name: "single segment tag", args: args{tag: "check"}, want: "-check"},
		{
			name: "multi segment tag",
			args: args{tag: "service-gaiad,service-start"},
			want: "-start",
		},
		{
			name: "trailing dash yields empty suffix",
			args: args{tag: "service-"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tagPrefix(tt.args.tag); got != tt.want {
				t.Errorf("tagPrefix() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_checkModeArgs(t *testing.T) {
	type args struct {
		tag string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{name: "empty tag", args: args{tag: ""}, want: []string{"--check"}},
		{
			name: "with tag",
			args: args{tag: "service-gaiad"},
			want: []string{"-t", "service-gaiad", "--check"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkModeArgs(tt.args.tag); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("checkModeArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_actionModeArgs(t *testing.T) {
	type args struct {
		tag string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{name: "empty tag", args: args{tag: ""}, want: nil},
		{
			name: "with tag",
			args: args{tag: "service-gaiad"},
			want: []string{"-t", "service-gaiad"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := actionModeArgs(tt.args.tag); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("actionModeArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_moleculeArgs(t *testing.T) {
	type args struct {
		scenario string
		stage    string
		extra    []string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{
			name: "no extra args",
			args: args{scenario: "default", stage: "create", extra: nil},
			want: []string{"molecule", "-v", "create", "-s", "default"},
		},
		{
			name: "with extra args",
			args: args{scenario: "default", stage: "converge", extra: []string{"--check"}},
			want: []string{"molecule", "-v", "converge", "-s", "default", "--", "--check"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moleculeArgs(tt.args.scenario, tt.args.stage, tt.
				args.extra); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("moleculeArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_runMolecule(t *testing.T) {
	t.Setenv("ANSIBLE_ROLES_PATH", "")
	dir := t.TempDir()
	okBin := writeFakeBin(t, dir, "molecule-ok", 0)
	failBin := writeFakeBin(t, dir, "molecule-fail", 3)
	type args struct {
		ctx       context.Context
		bin       string
		logPath   string
		rolesPath string
		args      []string
	}
	tests := []struct {
		name       string
		args       args
		envRoles   string
		wantStdout string
		wantErr    bool
	}{
		{
			name: "success streams args and log path",
			args: args{
				ctx: context.Background(), bin: okBin, logPath: "/tmp/molecule-x-0create",
				args: []string{"molecule", "-v", "create", "-s", "default"},
			},
			wantStdout: "args: molecule -v create -s default\n" +
				"ANSIBLE_LOG_PATH=/tmp/molecule-x-0create\n" +
				"ANSIBLE_ROLES_PATH=\n",
		},
		{
			name:    "non zero exit propagates error",
			args:    args{ctx: context.Background(), bin: failBin, logPath: "/tmp/x"},
			wantErr: true,
		},
		{
			name: "prepends roles path when existing is empty",
			args: args{
				ctx: context.Background(), bin: okBin, logPath: "/tmp/log",
				rolesPath: "/bar",
			},
			wantStdout: "args: \n" +
				"ANSIBLE_LOG_PATH=/tmp/log\n" +
				"ANSIBLE_ROLES_PATH=/bar\n",
		},
		{
			name: "prepends roles path to existing",
			args: args{
				ctx: context.Background(), bin: okBin, logPath: "/tmp/log",
				rolesPath: "/bar",
			},
			envRoles: "/foo",
			wantStdout: "args: \n" +
				"ANSIBLE_LOG_PATH=/tmp/log\n" +
				"ANSIBLE_ROLES_PATH=/bar:/foo\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ANSIBLE_ROLES_PATH", tt.envRoles)
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			err := runMolecule(
				tt.args.ctx, tt.args.bin, stdout, stderr, tt.args.logPath,
				tt.args.rolesPath, tt.args.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("runMolecule() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotStdout := stdout.String(); gotStdout != tt.wantStdout {
				t.Errorf("runMolecule() = %q, want %q", gotStdout, tt.wantStdout)
			}
		})
	}
}

func Test_uuidV7(t *testing.T) {
	t.Run("returns a uuid string", func(t *testing.T) {
		got, err := uuidV7()
		if err != nil {
			t.Fatalf("uuidV7() error = %v", err)
		}
		if _, err := uuid.Parse(got); err != nil {
			t.Errorf("uuidV7() = %q, not a uuid: %v", got, err)
		}
	})
	t.Run("rand failure propagates", func(t *testing.T) {
		uuid.SetRand(bytes.NewReader(nil))
		defer uuid.SetRand(nil)
		_, err := uuidV7()
		if err == nil {
			t.Error("expected error when rand.Reader fails")
		}
	})
}

// writeFakeGit creates an executable shell script mimicking `git clone`: it
// creates the destination directory (the last argument) and exits 0, or
// exits with code when code != 0 without creating anything.
func writeFakeGit(t *testing.T, dir, name string, code int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\n" +
		"echo \"git: $*\"\n" +
		"[ " + strconv.Itoa(code) + " -eq 0 ] && mkdir -p \"$5\"\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake git %s: %v", name, err)
	}
	return path
}

func TestCloneRoleRefs(t *testing.T) {
	const envVar = "GOTESTS_ROLE_REF"
	tests := []struct {
		name       string
		env        string
		buildOpts  func(dir, rolesPath string) CloneRoleRefsOptions
		wantCloned bool
		wantErr    bool
	}{
		{
			name: "missing RolesPath errors",
			env:  "",
			buildOpts: func(_, _ string) CloneRoleRefsOptions {
				return CloneRoleRefsOptions{
					Refs: []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
				}
			},
			wantErr: true,
		},
		{
			name: "env var unset skips clone",
			env:  "",
			buildOpts: func(dir, rolesPath string) CloneRoleRefsOptions {
				return CloneRoleRefsOptions{
					Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
					RolesPath: rolesPath, GitBinary: writeFakeGit(t, dir, "git-ok", 0),
				}
			},
			wantCloned: false,
		},
		{
			name: "env var set clones into roles path",
			env:  "v1.2.3",
			buildOpts: func(dir, rolesPath string) CloneRoleRefsOptions {
				return CloneRoleRefsOptions{
					Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
					RolesPath: rolesPath, GitBinary: writeFakeGit(t, dir, "git-ok", 0),
				}
			},
			wantCloned: true,
		},
		{
			name: "default git binary is used when empty",
			env:  "",
			buildOpts: func(_, rolesPath string) CloneRoleRefsOptions {
				return CloneRoleRefsOptions{
					Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
					RolesPath: rolesPath,
				}
			},
			wantCloned: false,
		},
		{
			name: "clone failure propagates",
			env:  "v1.2.3",
			buildOpts: func(dir, rolesPath string) CloneRoleRefsOptions {
				return CloneRoleRefsOptions{
					Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
					RolesPath: rolesPath, GitBinary: writeFakeGit(t, dir, "git-fail", 1),
				}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envVar, tt.env)
			dir := t.TempDir()
			rolesPath := filepath.Join(dir, "roles")
			opts := tt.buildOpts(dir, rolesPath)
			err := CloneRoleRefs(context.Background(), opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CloneRoleRefs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			_, statErr := os.Stat(filepath.Join(rolesPath, "role"))
			cloned := statErr == nil
			if cloned != tt.wantCloned {
				t.Errorf("CloneRoleRefs() cloned = %v, want %v", cloned, tt.wantCloned)
			}
		})
	}
	t.Run("removes stale role before cloning", func(t *testing.T) {
		t.Setenv(envVar, "v1.2.3")
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles")
		stale := filepath.Join(rolesPath, "role")
		if err := os.MkdirAll(filepath.Join(stale, "stale-marker"), 0o755); err != nil {
			t.Fatalf("seed stale role: %v", err)
		}
		err := CloneRoleRefs(context.Background(), CloneRoleRefsOptions{
			Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
			RolesPath: rolesPath, GitBinary: writeFakeGit(t, dir, "git-ok", 0),
		})
		if err != nil {
			t.Fatalf("CloneRoleRefs() error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(stale, "stale-marker")); err == nil {
			t.Errorf("stale role marker still present after clone")
		}
	})
	t.Run("remove stale role failure propagates", func(t *testing.T) {
		t.Setenv(envVar, "v1.2.3")
		dir := t.TempDir()
		rolesPath := filepath.Join(dir, "roles")
		if err := os.MkdirAll(rolesPath, 0o755); err != nil {
			t.Fatalf("seed roles path: %v", err)
		}
		stale := filepath.Join(rolesPath, "role")
		if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed stale role file: %v", err)
		}
		if err := os.Chmod(rolesPath, 0o555); err != nil {
			t.Fatalf("chmod read-only: %v", err)
		}
		defer func() { _ = os.Chmod(rolesPath, 0o755) }()
		err := CloneRoleRefs(context.Background(), CloneRoleRefsOptions{
			Refs:      []RoleRef{{EnvVar: envVar, RepoURL: "unused", RoleDir: "role"}},
			RolesPath: rolesPath, GitBinary: writeFakeGit(t, dir, "git-ok", 0),
		})
		if err == nil {
			t.Error("expected error when stale role cannot be removed")
		}
	})
}
