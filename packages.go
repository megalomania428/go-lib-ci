// Package ci is documented in doc.go.
// cspell:ignore dpkg noninteractive
package ci

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// defaultDpkgStatusFile is the path to the dpkg status database used when
// EnsurePackagesOptions.DpkgStatusFile is empty.
const defaultDpkgStatusFile = "/var/lib/dpkg/status"

// DefaultAptLockPath is used when EnsurePackagesOptions.LockPath is empty and
// the CI_APT_LOCK_PATH env variable is not set.
const DefaultAptLockPath = "/var/lock/go-lib-ci-apt.lock"

// AptLockPathEnv overrides DefaultAptLockPath at run time.
const AptLockPathEnv = "CI_APT_LOCK_PATH"

// EnsurePackagesOptions controls EnsurePackages behavior.
type EnsurePackagesOptions struct {
	// DpkgStatusFile overrides the path to the dpkg status database.
	// When empty, /var/lib/dpkg/status is used.
	DpkgStatusFile string
	// LockPath is the path used for the inter-process advisory lock guarding
	// concurrent apt-get invocations from multiple copies of this library.
	// When empty, AptLockPathEnv or DefaultAptLockPath is used.
	LockPath string
	// Stdout receives the apt-get stdout. Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives the apt-get stderr and progress lines. Defaults to
	// os.Stderr.
	Stderr io.Writer
	// SudoBinary overrides the sudo binary. Defaults to "sudo".
	SudoBinary string
	// AptGetBinary overrides the apt-get binary. Defaults to "apt-get".
	AptGetBinary string
}

// EnsurePackages verifies that every requested Debian package is installed,
// installing the missing ones via `sudo apt-get`. Multiple concurrent callers
// are serialized through a file lock at LockPath.
func EnsurePackages(ctx context.Context, packages []string,
	opts EnsurePackagesOptions) error {
	if len(packages) == 0 {
		return nil
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	statusFile := opts.DpkgStatusFile
	if statusFile == "" {
		statusFile = defaultDpkgStatusFile
	}
	installed, err := readInstalledPackages(statusFile)
	if err != nil {
		return fmt.Errorf("read dpkg status: %w", err)
	}
	seen := make(map[string]struct{}, len(packages))
	var missing []string
	for _, pkg := range packages {
		if pkg == "" {
			return fmt.Errorf("empty package name provided")
		}
		if _, ok := seen[pkg]; ok {
			continue
		}
		seen[pkg] = struct{}{}
		fmt.Fprintf(stdout, "==> package [%s]… ", pkg)
		if installed[pkg] {
			fmt.Fprintln(stdout, "exist")
			continue
		}
		fmt.Fprintln(stdout, "install")
		missing = append(missing, pkg)
	}
	if len(missing) == 0 {
		return nil
	}
	lockPath := opts.LockPath
	if lockPath == "" {
		lockPath = os.Getenv(AptLockPathEnv)
	}
	if lockPath == "" {
		lockPath = DefaultAptLockPath
	}
	unlock, err := acquireFileLock(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("acquire apt lock %s: %w", lockPath, err)
	}
	defer unlock()
	sudo := opts.SudoBinary
	if sudo == "" {
		sudo = "sudo"
	}
	apt := opts.AptGetBinary
	if apt == "" {
		apt = "apt-get"
	}
	if err := runAptGet(ctx, sudo, apt, stdout, stderr, "update"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	args := append([]string{"install", "--no-install-recommends", "-y", "--"}, missing...)
	if err := runAptGet(ctx, sudo, apt, stdout, stderr, args...); err != nil {
		return fmt.Errorf("apt-get install: %w", err)
	}
	return nil
}

func runAptGet(
	ctx context.Context, sudo, apt string, stdout, stderr io.Writer, args ...string,
) error {
	cmd := exec.CommandContext(ctx, sudo, append([]string{apt}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := killFn // snapshot before goroutine to avoid races with test hooks
	cmd.Cancel = func() error {
		if err := kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	environ := os.Environ()
	env := make([]string, 0, len(environ)+1)
	for _, e := range environ {
		if !strings.HasPrefix(e, "DEBIAN_FRONTEND=") {
			env = append(env, e)
		}
	}
	env = append(env, "DEBIAN_FRONTEND=noninteractive")
	cmd.Env = env
	return cmd.Run()
}

// readInstalledPackages parses the dpkg status file and returns a set of the
// packages whose Status field is "install ok installed".
func readInstalledPackages(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	installed := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var name, status string
	flush := func() {
		if name != "" && status == "install ok installed" {
			installed[name] = true
		}
		name, status = "", ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "Package: ") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
			continue
		}
		if strings.HasPrefix(line, "Status: ") {
			status = strings.TrimSpace(strings.TrimPrefix(line, "Status: "))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return installed, nil
}

// processLocks serializes access to the same lock path inside one process.
var (
	processLocksMu sync.Mutex
	processLocks   = map[string]*sync.Mutex{}
	// flockFn is called to acquire/release the kernel advisory lock. Overridden
	// in tests to simulate flock errors without touching the real syscall.
	flockFn = unix.Flock
	// killFn sends a signal to a process group. Overridden in tests to simulate
	// ESRCH without spawning a real process.
	killFn = syscall.Kill
)

func acquireFileLock(ctx context.Context, path string) (func(), error) {
	processLocksMu.Lock()
	m, ok := processLocks[path]
	if !ok {
		m = &sync.Mutex{}
		processLocks[path] = m
	}
	processLocksMu.Unlock()
	m.Lock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		m.Unlock()
		return nil, err
	}
	// Snapshot fd before spawning the goroutine: f.Fd() is not safe to call
	// concurrently with f.Close(), so we must not call it from two goroutines.
	fd := int(f.Fd())
	flock := flockFn // snapshot before goroutine to avoid races with test hooks
	done := make(chan error, 1)
	go func() { done <- flock(fd, unix.LOCK_EX) }()
	select {
	case err := <-done:
		if err != nil {
			_ = f.Close()
			m.Unlock()
			return nil, err
		}
	case <-ctx.Done():
		// Close() does not interrupt a blocking flock in another goroutine on
		// Linux. Let the goroutine finish on its own, then release the lock and
		// close the fd so the fd is not reused while the goroutine is alive.
		go func() {
			<-done
			_ = flock(fd, unix.LOCK_UN)
			_ = f.Close()
		}()
		m.Unlock()
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = flock(fd, unix.LOCK_UN)
			_ = f.Close()
			m.Unlock()
		})
	}, nil
}
