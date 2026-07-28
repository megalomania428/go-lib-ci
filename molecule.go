// Package ci is documented in doc.go.
// cspell:ignore appimage roles2test dpkg dconverge
package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultAppImageName is used when PrepareOptions.AppImageName is empty.
const defaultAppImageName = "ansible-11-001.AppImage"

// appImageRepo is the GitHub repository that publishes the ansible AppImage
// releases consumed by Prepare.
const appImageRepo = "raven428/container-images"

// GitHubTokenEnv is the environment variable checked by Prepare when
// PrepareOptions.RequireGitHubToken is set, and used (together with
// GITHUB_TOKEN) to authenticate the AppImage download.
const GitHubTokenEnv = "ANSIBLE_GITHUB_TOKEN"

// defaultRolesPath is the ANSIBLE_ROLES_PATH used when PrepareOptions.Isolated
// is false, reproducing the historical shared-host layout.
const defaultRolesPath = "/tmp/ansible/roles2test"

// defaultLogBase is the prefix (before the timestamp) used for
// ANSIBLE_LOG_PATH when PrepareOptions.Isolated is false.
const defaultLogBase = "/tmp/molecule"

// defaultIsolatedBase is used as the parent of the per-run temporary
// directory when PrepareOptions.Isolated is true and neither TmpBase nor the
// RUNNER_TEMP environment variable are set.
const defaultIsolatedBase = "/var/tmp"

// runnerTempEnv is the GitHub Actions variable pointing at a per-job
// temporary directory, preferred over defaultIsolatedBase when present.
const runnerTempEnv = "RUNNER_TEMP"

// timestampLayout mirrors the shell `date '+%Y%m%d%H%M%S.%3N'` format used by
// the historical run-tests.sh scripts.
const timestampLayout = "20060102150405.000"

// timeNow is overridden in tests for deterministic timestamps.
var timeNow = time.Now

// mkdirTempFn is overridden in tests to inject failures without touching the
// real filesystem behavior.
var mkdirTempFn = os.MkdirTemp

// osGetwdFn is overridden in tests to inject failures without touching the
// real working directory.
var osGetwdFn = os.Getwd

// PrepareOptions controls Prepare.
type PrepareOptions struct {
	// RoleDir is the symlink name created under the resolved
	// ANSIBLE_ROLES_PATH, pointing at SourceDir. It is the historical role
	// directory name, e.g. "ansible-mega-base".
	RoleDir string
	// SourceDir is the directory symlinked into ANSIBLE_ROLES_PATH/RoleDir.
	// When empty, the current working directory is used.
	SourceDir string
	// AppImageName is the exact AppImage asset file name to fetch. When
	// empty, defaultAppImageName is used.
	AppImageName string
	// AppImageRelease selects the GitHub release: "latest" (the default when
	// empty) or an explicit tag.
	AppImageRelease string
	// RequireGitHubToken makes Prepare fail early when GitHubTokenEnv is not
	// set, mirroring the mandatory-variable check in prepare.sh.
	RequireGitHubToken bool
	// EnsureMoreutils installs the moreutils package (needed by callers that
	// rely on `sponge`) via EnsurePackages before continuing.
	EnsureMoreutils bool
	// Isolated switches ANSIBLE_ROLES_PATH and the molecule log prefix to a
	// per-run unique directory, safe for multiple concurrent runners on one
	// host. When false, the historical shared paths are reused verbatim,
	// which is preferable for manual/local runs since it avoids re-fetching
	// galaxy dependencies on every invocation.
	Isolated bool
	// TmpBase overrides the parent directory used to create the per-run
	// unique directory when Isolated is true. When empty, the runnerTempEnv
	// environment variable is used, falling back to defaultIsolatedBase.
	TmpBase string
	// TmpPrefix names the per-run unique directory when Isolated is true,
	// e.g. "mega-base" yields "/var/tmp/mega-base-tests-<random>". Required
	// when Isolated is true.
	TmpPrefix string
	// HomeDir overrides $HOME for locating the downloaded AppImage. When
	// empty, the HOME environment variable is used.
	HomeDir string
	// FetchOverride, when set, is called with the FetchOptions Prepare is
	// about to use, allowing tests (or callers) to tweak fields such as
	// APIBaseURL, MaxRetriesTime or NoProgressBar.
	FetchOverride func(*FetchOptions)
	// PackagesOverride, when set, is called with the EnsurePackagesOptions
	// Prepare is about to use for EnsureMoreutils, allowing tests (or
	// callers) to tweak fields such as DpkgStatusFile or SudoBinary.
	PackagesOverride func(*EnsurePackagesOptions)
	// Stdout receives informational messages. Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives error and progress output. Defaults to os.Stderr.
	Stderr io.Writer
}

// PrepareResult carries the values a caller needs after a successful
// Prepare call.
type PrepareResult struct {
	// AppImageBin is the local path to the fetched ansible AppImage.
	AppImageBin string
	// RolesPath is the resolved ANSIBLE_ROLES_PATH.
	RolesPath string
	// LogBase is the ANSIBLE_LOG_PATH prefix, without the per-step suffix.
	LogBase string
	// IsolatedDir is the per-run unique directory created when
	// PrepareOptions.Isolated is true, and empty otherwise.
	IsolatedDir string
}

// Prepare performs the common molecule test setup shared by the raven428
// ansible role repositories: it optionally checks for a mandatory GitHub
// token, optionally ensures moreutils is installed, fetches the ansible
// AppImage, resolves ANSIBLE_ROLES_PATH (isolated or shared) and symlinks
// SourceDir under it.
func Prepare(ctx context.Context, opts PrepareOptions) (PrepareResult, error) {
	if opts.RoleDir == "" {
		return PrepareResult{}, errors.New("molecule: RoleDir is required")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if opts.RequireGitHubToken && os.Getenv(GitHubTokenEnv) == "" {
		return PrepareResult{}, fmt.Errorf(
			"mandatory variable [%s] not found", GitHubTokenEnv)
	}
	if opts.EnsureMoreutils {
		pkgOpts := EnsurePackagesOptions{Stdout: stdout, Stderr: stderr}
		if opts.PackagesOverride != nil {
			opts.PackagesOverride(&pkgOpts)
		}
		if err := EnsurePackages(ctx, []string{"moreutils"}, pkgOpts); err != nil {
			return PrepareResult{}, fmt.Errorf("ensure moreutils: %w", err)
		}
	}
	rolesPath, logBase, isolatedDir, err := resolveMoleculePaths(opts)
	if err != nil {
		return PrepareResult{}, err
	}
	appImageBin, err := fetchAppImage(ctx, opts, stdout, stderr)
	if err != nil {
		return PrepareResult{}, err
	}
	sourceDir := opts.SourceDir
	if sourceDir == "" {
		if sourceDir, err = osGetwdFn(); err != nil {
			return PrepareResult{}, fmt.Errorf("resolve working directory: %w", err)
		}
	}
	if err := linkRole(rolesPath, opts.RoleDir, sourceDir, stdout); err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{
		AppImageBin: appImageBin,
		RolesPath:   rolesPath,
		LogBase:     logBase,
		IsolatedDir: isolatedDir,
	}, nil
}

// fetchAppImage resolves the AppImage name/release defaults and downloads it
// via FetchGitHubRelease, reusing its retry, digest-verification and
// progress-bar behavior instead of duplicating the historical plain curl
// download.
func fetchAppImage(
	ctx context.Context, opts PrepareOptions, stdout, stderr io.Writer,
) (string, error) {
	name := opts.AppImageName
	if name == "" {
		name = defaultAppImageName
	}
	release := opts.AppImageRelease
	if release == "" {
		release = "latest"
	}
	tag := release
	if tag == "latest" {
		tag = ""
	}
	home := opts.HomeDir
	if home == "" {
		home = os.Getenv("HOME")
	}
	fetch := FetchOptions{
		Repo:      appImageRepo,
		Tag:       tag,
		AssetName: name,
		DestDir:   filepath.Join(home, "bin"),
		Token: firstNonEmpty(
			os.Getenv(GitHubTokenEnv), os.Getenv("GITHUB_TOKEN"),
		),
		FallbackToExisting: true,
		Stdout:             stdout,
		Stderr:             stderr,
	}
	if opts.FetchOverride != nil {
		opts.FetchOverride(&fetch)
	}
	path, err := FetchGitHubRelease(ctx, fetch)
	if err != nil {
		return "", fmt.Errorf("fetch ansible appimage: %w", err)
	}
	return path, nil
}

// resolveMoleculePaths computes ANSIBLE_ROLES_PATH and the ANSIBLE_LOG_PATH
// prefix, either reusing the historical shared-host paths or creating a
// unique per-run directory, per PrepareOptions.Isolated.
func resolveMoleculePaths(opts PrepareOptions) (rolesPath, logBase, isolatedDir string,
	err error) {
	if !opts.Isolated {
		return defaultRolesPath, defaultLogBase + "-" + timeNow().Format(timestampLayout),
			"", nil
	}
	if opts.TmpPrefix == "" {
		return "", "", "", errors.New("molecule: TmpPrefix is required when Isolated is true")
	}
	base := opts.TmpBase
	if base == "" {
		base = os.Getenv(runnerTempEnv)
	}
	if base == "" {
		base = defaultIsolatedBase
	}
	dir, err := mkdirTempFn(base, opts.TmpPrefix+"-tests-*")
	if err != nil {
		return "", "", "", fmt.Errorf("create isolated tmp dir: %w", err)
	}
	rolesPath = filepath.Join(dir, "roles2test")
	logBase = filepath.Join(dir, "molecule-"+timeNow().Format(timestampLayout))
	return rolesPath, logBase, dir, nil
}

// linkRole creates rolesPath if missing, then replaces rolesPath/roleDir with
// a fresh symlink pointing at sourceDir.
func linkRole(rolesPath, roleDir, sourceDir string, stdout io.Writer) error {
	if err := os.MkdirAll(rolesPath, 0o755); err != nil {
		return fmt.Errorf("create roles path %s: %w", rolesPath, err)
	}
	target := filepath.Join(rolesPath, roleDir)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove stale role link %s: %w", target, err)
	}
	if err := os.Symlink(sourceDir, target); err != nil {
		return fmt.Errorf("link role %s -> %s: %w", target, sourceDir, err)
	}
	fmt.Fprintf(stdout, "==> role linked [%s] -> [%s]\n", target, sourceDir)
	return nil
}

// firstNonEmpty returns the first non-empty string among values.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// MoleculeCreateOptions controls MoleculeCreate.
type MoleculeCreateOptions struct {
	// MoleculeBinary is the ansible AppImage (or any binary exposing a
	// `molecule` subcommand) to invoke. Required.
	MoleculeBinary string
	// Scenario is the molecule scenario name, e.g. "default".
	Scenario string
	// LogBase is the ANSIBLE_LOG_PATH prefix, typically
	// PrepareResult.LogBase.
	LogBase string
	// Stdout receives the printf-style step banner and the command stdout.
	// Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives the command stderr. Defaults to os.Stderr.
	Stderr io.Writer
}

// MoleculeCreate runs `molecule create` for the given scenario, mirroring the
// "molecule [create] action" step shared by every run-tests.sh script.
func MoleculeCreate(ctx context.Context, opts MoleculeCreateOptions) error {
	if opts.MoleculeBinary == "" {
		return errors.New("molecule: MoleculeBinary is required")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprint(stdout, "\n\n\nmolecule [create] action\n")
	logPath := opts.LogBase + "-0create"
	return runMolecule(ctx, opts.MoleculeBinary, stdout, stderr, logPath,
		"molecule", "-v", "create", "-s", opts.Scenario)
}

// RunGroupOptions controls RunGroup.
type RunGroupOptions struct {
	// MoleculeBinary is the ansible AppImage (or any binary exposing a
	// `molecule` subcommand) to invoke. Required.
	MoleculeBinary string
	// Scenario is the molecule scenario name, e.g. "default".
	Scenario string
	// Tag restricts the run to a molecule/ansible tag, e.g.
	// "service-gaiad,service-start". Empty runs the whole play.
	Tag string
	// LogBase is the ANSIBLE_LOG_PATH prefix, typically
	// PrepareResult.LogBase.
	LogBase string
	// Counter is the shared step counter (bash `n`), incremented after every
	// molecule invocation. Required, and must be reused across sequential
	// RunGroup/MoleculeCreate calls belonging to the same run so that
	// ANSIBLE_LOG_PATH suffixes stay strictly increasing, matching the
	// historical run-tests.sh behavior.
	Counter *int
	// Stdout receives the printf-style step banners and the command stdout.
	// Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives the command stderr. Defaults to os.Stderr.
	Stderr io.Writer
}

// RunGroup reproduces the run_group() shell function shared by
// run-tests.sh: a converge --check pass, followed by converge and
// idempotence for both the "action" and "--check" modes.
func RunGroup(ctx context.Context, opts RunGroupOptions) error {
	if opts.MoleculeBinary == "" {
		return errors.New("molecule: MoleculeBinary is required")
	}
	if opts.Counter == nil {
		return errors.New("molecule: Counter is required")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	prefix := tagPrefix(opts.Tag)
	checkArgs := checkModeArgs(opts.Tag)
	label := opts.Tag
	if label == "" {
		label = "empty"
	}
	fmt.Fprintf(stdout, "\n\n\nmolecule [converge] %s check\n", label)
	logPath := fmt.Sprintf("%s-%02dconverge%s-check", opts.LogBase, *opts.Counter, prefix)
	if err := runMolecule(ctx, opts.MoleculeBinary, stdout, stderr, logPath,
		moleculeArgs(opts.Scenario, "converge", checkArgs)...); err != nil {
		return err
	}
	*opts.Counter++
	actionArgs := actionModeArgs(opts.Tag)
	for _, mode := range []string{"action", "check"} {
		args := actionArgs
		if mode == "check" {
			args = checkArgs
		}
		for _, stage := range []string{"converge", "idempotence"} {
			fmt.Fprintf(stdout, "\n\n\nmolecule [%s] %s %s\n", stage, mode, label)
			logPath = fmt.Sprintf(
				"%s-%02d%s%s-%s", opts.LogBase, *opts.Counter, stage, prefix, mode)
			if err := runMolecule(ctx, opts.MoleculeBinary, stdout, stderr, logPath,
				moleculeArgs(opts.Scenario, stage, args)...); err != nil {
				return err
			}
			*opts.Counter++
		}
	}
	return nil
}

// tagPrefix reproduces `prefix="${tag##*-}"; [[ -n prefix ]] && prefix="-${prefix}"`:
// the part of tag after its last '-', prefixed with '-' when non-empty.
func tagPrefix(tag string) string {
	suffix := tag
	if i := strings.LastIndexByte(tag, '-'); i >= 0 {
		suffix = tag[i+1:]
	}
	if suffix == "" {
		return ""
	}
	return "-" + suffix
}

// checkModeArgs returns the molecule CLI arguments appended after "--" for a
// --check invocation: ["--check"] when tag is empty, or
// ["-t", tag, "--check"] otherwise.
func checkModeArgs(tag string) []string {
	if tag == "" {
		return []string{"--check"}
	}
	return []string{"-t", tag, "--check"}
}

// actionModeArgs returns the molecule CLI arguments appended after "--" for
// an action (non-check) invocation: nil when tag is empty, or ["-t", tag]
// otherwise.
func actionModeArgs(tag string) []string {
	if tag == "" {
		return nil
	}
	return []string{"-t", tag}
}

// moleculeArgs builds the full molecule CLI argument list, appending "--"
// followed by extra only when extra is non-empty, matching the shell
// behavior where an empty $args expands to nothing.
func moleculeArgs(scenario, stage string, extra []string) []string {
	args := []string{"molecule", "-v", stage, "-s", scenario}
	if len(extra) > 0 {
		args = append(args, "--")
		args = append(args, extra...)
	}
	return args
}

// runMolecule executes bin with args, setting ANSIBLE_LOG_PATH=logPath in its
// environment and streaming its output to stdout/stderr.
func runMolecule(
	ctx context.Context, bin string, stdout, stderr io.Writer, logPath string,
	args ...string,
) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "ANSIBLE_LOG_PATH="+logPath)
	return cmd.Run()
}

// RoleRef describes one optional role override: when the environment
// variable named EnvVar is set, the repository at RepoURL is cloned at that
// branch/ref into RolesPath/RoleDir, replacing any galaxy-installed copy.
type RoleRef struct {
	// EnvVar is the environment variable holding the git ref to clone, e.g.
	// "MEGA_VAR_REF".
	EnvVar string
	// RepoURL is the git repository to clone, e.g.
	// "https://github.com/raven428/ansible-mega-var.git".
	RepoURL string
	// RoleDir is the directory name created under RolesPath, e.g.
	// "raven428.mega_var".
	RoleDir string
}

// CloneRoleRefsOptions controls CloneRoleRefs.
type CloneRoleRefsOptions struct {
	// Refs lists the role overrides to evaluate, in order.
	Refs []RoleRef
	// RolesPath is the ANSIBLE_ROLES_PATH under which each RoleRef.RoleDir is
	// cloned, typically PrepareResult.RolesPath.
	RolesPath string
	// GitBinary overrides the git binary. Defaults to "git".
	GitBinary string
	// Stdout receives informational messages. Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives the git command stderr. Defaults to os.Stderr.
	Stderr io.Writer
}

// CloneRoleRefs overrides galaxy-installed roles with specific git refs when
// requested via environment variables, reproducing the repeated
// "if [[ -n ${*_REF:-} ]]; then git clone --branch ..." blocks found in
// run-tests.sh across the launch/service/mini-vault repositories. It must run
// after molecule create, since that is what triggers the galaxy dependency
// install being overridden.
func CloneRoleRefs(ctx context.Context, opts CloneRoleRefsOptions) error {
	if opts.RolesPath == "" {
		return errors.New("molecule: RolesPath is required")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	git := opts.GitBinary
	if git == "" {
		git = "git"
	}
	for _, ref := range opts.Refs {
		val := os.Getenv(ref.EnvVar)
		if val == "" {
			fmt.Fprintf(stdout, "%s input missed\n", ref.EnvVar)
			continue
		}
		fmt.Fprintf(stdout, "%s=%s\n", ref.EnvVar, val)
		target := filepath.Join(opts.RolesPath, ref.RoleDir)
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove stale role %s: %w", target, err)
		}
		cmd := exec.CommandContext(ctx, git,
			"clone", "--branch", val, ref.RepoURL, target)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git clone %s: %w", ref.RepoURL, err)
		}
	}
	return nil
}
