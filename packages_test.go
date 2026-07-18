// cspell:ignore dpkg deinstall
package ci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const dpkgSample = `Package: bash
Status: install ok installed
Version: 5.2

Package: jq
Status: deinstall ok config-files
Version: 1.6

Package: fuse3
Status: install ok installed
Version: 3.14
`

func TestReadInstalledPackages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	if err := os.WriteFile(path, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	got, err := readInstalledPackages(path)
	if err != nil {
		t.Fatalf("readInstalledPackages: %v", err)
	}
	want := map[string]bool{"bash": true, "fuse3": true}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing installed package %s", k)
		}
	}
	if got["jq"] {
		t.Errorf("jq must not be reported as installed (deinstall state)")
	}
}

// writeFakeSudo creates an executable shell script at dir/name that exits with
// the given code and prints its arguments to stdout.
func writeFakeSudo(t *testing.T, dir, name string, code int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\necho \"sudo: $*\"\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake sudo %s: %v", name, err)
	}
	return path
}

func TestEnsurePackages(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	fakeOK := writeFakeSudo(t, dir, "sudo-ok", 0)
	fakeFail := writeFakeSudo(t, dir, "sudo-fail", 7)
	lockPath := filepath.Join(dir, "test.lock")

	missingStatusPath := filepath.Join(dir, "missing-status")

	type args struct {
		ctx      context.Context
		packages []string
		opts     EnsurePackagesOptions
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name: "empty package list returns nil",
			args: args{
				ctx:      context.Background(),
				packages: nil,
				opts:     EnsurePackagesOptions{DpkgStatusFile: statusPath},
			},
			wantErr: false,
		},
		{
			name: "all packages already installed",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash", "fuse3"},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: statusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
				},
			},
			wantErr: false,
		},
		{
			name: "install missing via fake sudo",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash", "missing-pkg"},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: statusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
					LockPath:       lockPath,
					SudoBinary:     fakeOK,
					AptGetBinary:   "apt-get",
				},
			},
			wantErr: false,
		},
		{
			name: "update failure propagates apt-get error",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash", "missing-pkg"},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: statusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
					LockPath:       lockPath,
					SudoBinary:     fakeFail,
					AptGetBinary:   "apt-get",
				},
			},
			wantErr: true,
			errSub:  "apt-get update",
		},
		{
			name: "dpkg status unreadable",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash"},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: missingStatusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
				},
			},
			wantErr: true,
			errSub:  "read dpkg status",
		},
		{
			name: "empty package name returns error",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash", ""},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: statusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
				},
			},
			wantErr: true,
			errSub:  "empty package name",
		},
		{
			name: "duplicate packages deduplicated",
			args: args{
				ctx:      context.Background(),
				packages: []string{"bash", "fuse3", "bash"},
				opts: EnsurePackagesOptions{
					DpkgStatusFile: statusPath,
					Stdout:         io.Discard,
					Stderr:         io.Discard,
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := EnsurePackages(tt.args.ctx, tt.args.packages, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsurePackages() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("EnsurePackages() error = %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func TestEnsurePackagesLockPathEnv(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	envLock := filepath.Join(dir, "env.lock")
	t.Setenv(AptLockPathEnv, envLock)
	fakeOK := writeFakeSudo(t, dir, "sudo-ok", 0)
	var stdout, stderr bytes.Buffer
	err := EnsurePackages(
		context.Background(), []string{"missing-pkg"}, EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
			Stdout:         &stdout,
			Stderr:         &stderr,
			SudoBinary:     fakeOK,
			AptGetBinary:   "apt-get",
		},
	)
	if err != nil {
		t.Fatalf("EnsurePackages via env lock: %v", err)
	}
	if _, err := os.Stat(envLock); err != nil {
		t.Errorf("env-based lock path not created: %v", err)
	}
	if !strings.Contains(stdout.String(), "install") {
		t.Errorf("expected install log, got: %q", stdout.String())
	}
}

func TestEnsurePackagesLockAcquireFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file lock semantics differ on windows")
	}

	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	// Lock path inside a non-directory file -> open fails with ENOTDIR.
	badLock := filepath.Join(blocker, "sub.lock")

	err := EnsurePackages(
		context.Background(), []string{"missing-pkg"}, EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
			Stdout:         io.Discard,
			Stderr:         io.Discard,
			LockPath:       badLock,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "acquire apt lock") {
		t.Fatalf("expected lock acquire error, got %v", err)
	}
}

func TestEnsurePackagesDefaultsStdoutStderr(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	err := EnsurePackages(
		context.Background(), []string{"bash"}, EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
		},
	)
	if err != nil {
		t.Fatalf("EnsurePackages default writers: %v", err)
	}
}

// TestEnsurePackagesDefaultStatusFile covers the defaultDpkgStatusFile fallback
// branch: empty DpkgStatusFile must resolve to the real dpkg status database.
func TestEnsurePackagesDefaultStatusFile(t *testing.T) {
	if _, err := os.Stat(defaultDpkgStatusFile); err != nil {
		t.Skipf("system dpkg status file not available: %v", err)
	}
	err := EnsurePackages(
		context.Background(), []string{"bash"}, EnsurePackagesOptions{},
	)
	if err != nil {
		t.Fatalf("EnsurePackages with default status file: %v", err)
	}
}

func Test_runAptGet(t *testing.T) {
	dir := t.TempDir()
	fakeOK := writeFakeSudo(t, dir, "sudo-ok", 0)
	fakeFail := writeFakeSudo(t, dir, "sudo-fail", 4)

	type args struct {
		ctx  context.Context
		sudo string
		apt  string
		args []string
	}
	tests := []struct {
		name       string
		args       args
		wantStdout string
		wantStderr string
		wantErr    bool
	}{
		{
			name: "successful run captures stdout",
			args: args{
				ctx:  context.Background(),
				sudo: fakeOK,
				apt:  "apt-get",
				args: []string{"update"},
			},
			wantStdout: "sudo: apt-get update\n",
			wantStderr: "",
			wantErr:    false,
		},
		{
			name: "missing sudo binary errors",
			args: args{
				ctx:  context.Background(),
				sudo: filepath.Join(dir, "does-not-exist"),
				apt:  "apt-get",
				args: []string{"update"},
			},
			wantStdout: "",
			wantStderr: "",
			wantErr:    true,
		},
		{
			name: "non-zero exit returns error",
			args: args{
				ctx:  context.Background(),
				sudo: fakeFail,
				apt:  "apt-get",
				args: []string{"install", "foo"},
			},
			wantStdout: "sudo: apt-get install foo\n",
			wantStderr: "",
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			err := runAptGet(
				tt.args.ctx, tt.args.sudo, tt.args.apt, stdout, stderr,
				tt.args.args...,
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("runAptGet() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotStdout := stdout.String(); gotStdout != tt.wantStdout {
				t.Errorf("runAptGet() stdout = %q, want %q", gotStdout, tt.wantStdout)
			}
			if gotStderr := stderr.String(); gotStderr != tt.wantStderr {
				t.Errorf("runAptGet() stderr = %q, want %q", gotStderr, tt.wantStderr)
			}
		})
	}
}

func Test_runAptGetCancel(t *testing.T) {
	dir := t.TempDir()
	// Script that sleeps long enough for context cancellation to fire.
	path := filepath.Join(dir, "sudo-sleep")
	body := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := runAptGet(ctx, path, "apt-get", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runAptGet() expected error on context cancel, got nil")
	}
}

func Test_runAptGetCancelKillError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("syscall.Kill not available on windows")
	}
	tests := []struct {
		name    string
		killErr error
		wantNil bool // cmd.Run returns nil when Cancel returns ErrProcessDone
		wantErr error
	}{
		{
			// ESRCH → os.ErrProcessDone tells exec the process is already done;
			// cmd.Run succeeds (returns nil) rather than surfacing a kill error.
			name:    "ESRCH maps to ErrProcessDone, Run returns nil",
			killErr: syscall.ESRCH,
			wantNil: true,
		},
		{
			name:    "other kill error propagates through Run",
			killErr: syscall.EPERM,
			wantErr: syscall.EPERM,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sudo-sleep")
			// Script exits on its own quickly; the mocked killFn never sends a
			// real signal, so the process must not block forever.
			if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
				t.Fatalf("write fake sudo: %v", err)
			}
			orig := killFn
			t.Cleanup(func() { killFn = orig })
			killFn = func(_ int, _ syscall.Signal) error { return tt.killErr }
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()
			err := runAptGet(ctx, path, "apt-get", io.Discard, io.Discard)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("runAptGet() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("runAptGet() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func Test_readInstalledPackages(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "status")
	if err := os.WriteFile(goodPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	missingPath := filepath.Join(dir, "missing-status")
	trailingPath := filepath.Join(dir, "trailing-status")
	if err := os.WriteFile(trailingPath,
		[]byte("Package: curl\nStatus: install ok installed\n"),
		0o644,
	); err != nil {
		t.Fatalf("write trailing sample: %v", err)
	}

	type args struct {
		path string
	}
	tests := []struct {
		name    string
		args    args
		want    map[string]bool
		wantErr bool
	}{
		{
			name:    "parses installed packages skipping deinstall",
			args:    args{path: goodPath},
			want:    map[string]bool{"bash": true, "fuse3": true},
			wantErr: false,
		},
		{
			name:    "missing file returns error",
			args:    args{path: missingPath},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "trailing record without blank line is flushed",
			args:    args{path: trailingPath},
			want:    map[string]bool{"curl": true},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readInstalledPackages(tt.args.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("readInstalledPackages() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readInstalledPackages() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_acquireFileLock(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "ok.lock")
	badParent := filepath.Join(dir, "blocker")
	if err := os.WriteFile(badParent, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	badPath := filepath.Join(badParent, "sub.lock") // open fails: ENOTDIR

	type args struct {
		ctx  context.Context
		path string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errSub  string
	}{
		{
			name:    "acquires and releases flock",
			args:    args{ctx: context.Background(), path: goodPath},
			wantErr: false,
		},
		{
			name: "context canceled before flock aborts",
			args: args{ctx: func() context.Context {
				c, cancel := context.WithCancel(context.Background())
				cancel()
				return c
			}(), path: goodPath},
			wantErr: true,
			errSub:  context.Canceled.Error(),
		},
		{
			name:    "open failure returns error",
			args:    args{ctx: context.Background(), path: badPath},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unlock, err := acquireFileLock(tt.args.ctx, tt.args.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("acquireFileLock() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("acquireFileLock() error = %v, want substring %q", err, tt.errSub)
				}
				return
			}
			if unlock == nil {
				t.Fatalf("unlock func is nil")
			}
			unlock()
			// Re-lock the same path to verify release worked.
			unlock2, err := acquireFileLock(context.Background(), tt.args.path)
			if err != nil {
				t.Fatalf("reacquire after release failed: %v", err)
			}
			unlock2()
		})
	}
}

func Test_acquireFileLockIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not available on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.lock")
	unlock, err := acquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Second call must not panic or deadlock.
	unlock()
	unlock()
}

func Test_acquireFileLockContention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not available on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "contention.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	unlock, err := acquireFileLock(ctx, path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second concurrent acquire must block until the holder releases.
	type result struct {
		unlock func()
		err    error
	}
	secondDone := make(chan result, 1)
	go func() {
		u, err := acquireFileLock(ctx, path)
		secondDone <- result{u, err}
	}()
	select {
	case <-secondDone:
		t.Fatalf("second acquire returned before release")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case res := <-secondDone:
		if res.err != nil {
			t.Fatalf("second acquire after release failed: %v", res.err)
		}
		res.unlock()
	case <-time.After(2 * time.Second):
		t.Fatalf("second acquire did not complete after release")
	}
}

// Test_readInstalledPackagesScannerError feeds a status file containing a
// single line longer than the 1 MiB scanner buffer, forcing scanner.Err to
// surface a non-nil error.
func Test_readInstalledPackagesScannerError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	huge := strings.Repeat("a", (1024*1024)+8)
	if err := os.WriteFile(path, []byte(huge), 0o644); err != nil {
		t.Fatalf("write huge: %v", err)
	}
	if _, err := readInstalledPackages(path); err == nil {
		t.Fatalf("expected scanner error from oversized line, got nil")
	}
}

// TestEnsurePackagesDefaultBinaries covers the `sudo == ""` and `apt == ""`
// fallback branches by putting fake `sudo` and `apt-get` binaries on PATH.
func TestEnsurePackagesDefaultBinaries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-oriented shell wrapper test")
	}

	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	fakeBin := filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, name := range []string{"sudo", "apt-get"} {
		p := filepath.Join(fakeBin, name)
		body := "#!/bin/sh\nexit 0\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	// Prepend our fake bin dir to PATH; runAptGet uses os.Environ.
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	err := EnsurePackages(
		context.Background(), []string{"bash", "missing-pkg"},
		EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
			Stdout:         &stdout,
			Stderr:         &stderr,
			LockPath:       filepath.Join(dir, "lock"),
		},
	)
	if err != nil {
		t.Fatalf("EnsurePackages default binaries: %v", err)
	}
	if !strings.Contains(stdout.String(), "install") {
		t.Errorf("expected install log, got: %q", stdout.String())
	}
}

// TestEnsurePackagesInstallError covers the `apt-get install` failure branch:
// the fake sudo exits 0 for `update` but non-zero for `install`.
func TestEnsurePackagesInstallError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-oriented shell wrapper test")
	}

	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	// sudo wrapper: exit 0 unless "install" appears in argv.
	sudoPath := filepath.Join(dir, "sudo")
	sudoBody := `#!/bin/sh
for a in "$@"; do if [ "$a" = "install" ]; then exit 9; fi; done
echo "sudo: $*"
exit 0
`
	if err := os.WriteFile(sudoPath, []byte(sudoBody), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	aptPath := filepath.Join(dir, "apt-get")
	if err := os.WriteFile(aptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake apt-get: %v", err)
	}
	err := EnsurePackages(
		context.Background(), []string{"bash", "missing-pkg"},
		EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
			Stdout:         io.Discard,
			Stderr:         io.Discard,
			LockPath:       filepath.Join(dir, "lock"),
			SudoBinary:     sudoPath,
			AptGetBinary:   aptPath,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "apt-get install") {
		t.Fatalf("expected apt-get install error, got %v", err)
	}
}

// TestEnsurePackagesDefaultLockPath covers the `lockPath == ""` branch falling
// back to DefaultAptLockPath when neither opts.LockPath nor env is set.
func TestEnsurePackagesDefaultLockPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-oriented shell wrapper test")
	}
	t.Setenv(AptLockPathEnv, "")

	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(statusPath, []byte(dpkgSample), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	fakeBin := filepath.Join(dir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, name := range []string{"sudo", "apt-get"} {
		p := filepath.Join(fakeBin, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	// DefaultAptLockPath is a const pointing to /var/lock/... which is not
	// writable as non-root, so the test asserts that lock acquisition fails
	// with an error mentioning the default path.
	err := EnsurePackages(
		context.Background(), []string{"bash", "missing-pkg"},
		EnsurePackagesOptions{
			DpkgStatusFile: statusPath,
			Stdout:         io.Discard,
			Stderr:         io.Discard,
		},
	)
	if err == nil {
		// On hosts where /var/lock happens to be writable, this succeeds.
		return
	}
	if !strings.Contains(err.Error(), "acquire apt lock") {
		t.Fatalf("expected lock acquire error referencing default path, got %v", err)
	}
	if !strings.Contains(err.Error(), DefaultAptLockPath) {
		t.Errorf("expected error to mention %q, got %v", DefaultAptLockPath, err)
	}
}

// Test_acquireFileLockFlockError covers the branch where flock itself returns
// an error (not caused by context cancellation and not an open failure).
func Test_acquireFileLockFlockError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flock-err.lock")
	sentinel := errors.New("injected flock error")
	orig := flockFn
	// Restore flockFn after test and remove the per-path mutex entry so
	t.Cleanup(func() {
		flockFn = orig
		// Remove the per-path mutex entry so other tests are not affected.
		processLocksMu.Lock()
		delete(processLocks, path)
		processLocksMu.Unlock()
	})
	flockFn = func(fd, how int) error {
		if how == unix.LOCK_EX {
			return sentinel
		}
		return orig(fd, how)
	}
	_, err := acquireFileLock(context.Background(), path)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel flock error, got %v", err)
	}
}
