// Package ci is documented in doc.go.
package ci

// cspell:ignore oneline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type gitConfig struct {
	binary, dir, url, ref, filter, base, head, match string
	depth, maxCount                                  int
	bare, mergeBase                                  bool
	paths                                            []string
	stderr                                           io.Writer
}

// GitOption configures the Git helpers.
type GitOption func(*gitConfig)

// WithGitBinary overrides the git executable.
func WithGitBinary(path string) GitOption {
	return func(c *gitConfig) { c.binary = path }
}

// WithGitDir sets the clone destination or working tree, defaulting to ".".
func WithGitDir(dir string) GitOption { return func(c *gitConfig) { c.dir = dir } }

// WithGitStderr sets the error stream.
func WithGitStderr(w io.Writer) GitOption {
	return func(c *gitConfig) { c.stderr = w }
}

// WithGitURL sets the clone URL.
func WithGitURL(url string) GitOption { return func(c *gitConfig) { c.url = url } }

// WithGitRef sets the tag or branch.
func WithGitRef(ref string) GitOption { return func(c *gitConfig) { c.ref = ref } }

// WithGitDepth sets clone depth; zero keeps full history.
func WithGitDepth(n int) GitOption { return func(c *gitConfig) { c.depth = n } }

// WithGitBare requests a bare clone.
func WithGitBare(bare bool) GitOption { return func(c *gitConfig) { c.bare = bare } }

// WithGitFilter sets a partial-clone filter such as blob:none.
func WithGitFilter(spec string) GitOption {
	return func(c *gitConfig) { c.filter = spec }
}

// WithGitBase sets the comparison base.
func WithGitBase(ref string) GitOption { return func(c *gitConfig) { c.base = ref } }

// WithGitHead sets the comparison head, defaulting to HEAD.
func WithGitHead(ref string) GitOption { return func(c *gitConfig) { c.head = ref } }

// WithGitMergeBase uses a triple-dot comparison in GitChangedFiles.
func WithGitMergeBase(on bool) GitOption {
	return func(c *gitConfig) { c.mergeBase = on }
}

// WithGitPaths restricts diff, log or show to paths.
func WithGitPaths(paths ...string) GitOption {
	return func(c *gitConfig) { c.paths = paths }
}

// WithGitMatch sets the previous-tag pattern.
func WithGitMatch(pattern string) GitOption {
	return func(c *gitConfig) { c.match = pattern }
}

// WithGitMaxCount limits log entries.
func WithGitMaxCount(n int) GitOption {
	return func(c *gitConfig) { c.maxCount = n }
}
func newGitConfig(opts []GitOption) *gitConfig {
	c := &gitConfig{binary: "git", dir: ".", head: "HEAD", stderr: os.Stderr}
	for _, opt := range opts {
		opt(c)
	}
	return c
}
func (c *gitConfig) output(ctx context.Context, args ...string) (string, error) {
	return c.outputEnv(ctx, nil, args...)
}
func (c *gitConfig) outputEnv(ctx context.Context, env []string,
	args ...string) (string, error) {
	return CommandOutput(ctx, WithCommand(c.binary, args...), WithCommandDir(c.dir),
		WithCommandStderr(c.stderr), WithCommandEnv(env...))
}

// GitClone clones URL at Ref into Dir, with optional depth, bare mode and filter.
func GitClone(ctx context.Context, opts ...GitOption) error {
	c := newGitConfig(opts)
	if c.url == "" {
		return fmt.Errorf("GitClone: URL: %w", ErrMissingOption)
	}
	if c.ref == "" {
		return fmt.Errorf("GitClone: ref: %w", ErrMissingOption)
	}
	args := []string{"clone", "--branch", c.ref}
	if c.depth > 0 {
		args = append(args, "--depth", strconv.Itoa(c.depth))
	}
	if c.bare {
		args = append(args, "--bare")
	}
	if c.filter != "" {
		args = append(args, "--filter="+c.filter)
	}
	args = append(args, "--", c.url, c.dir)
	return RunCommand(ctx, WithCommand(c.binary, args...), WithCommandStderr(c.stderr))
}

// GitChangedFiles returns paths changed between Base and Head.
func GitChangedFiles(ctx context.Context, opts ...GitOption) ([]string, error) {
	c := newGitConfig(opts)
	if c.base == "" {
		return nil, fmt.Errorf("GitChangedFiles: base: %w", ErrMissingOption)
	}
	sep := ".."
	if c.mergeBase {
		sep = "..."
	}
	out, err := c.output(ctx, "diff", "--name-only", "--no-renames", "-z",
		"--end-of-options", c.base+sep+c.head, "--")
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == 0 }), nil
}

// GitPreviousTag finds the nearest matching tag reachable from Ref, excluding Ref.
// It returns an empty string without an error when there is no matching tag.
func GitPreviousTag(ctx context.Context, opts ...GitOption) (string, error) {
	c := newGitConfig(opts)
	if c.ref == "" {
		return "", fmt.Errorf("GitPreviousTag: ref: %w", ErrMissingOption)
	}
	if c.match == "" {
		return "", fmt.Errorf("GitPreviousTag: match: %w", ErrMissingOption)
	}
	var diagnostic bytes.Buffer
	c.stderr = &diagnostic
	exclude := strings.TrimPrefix(c.ref, "refs/tags/")
	out, err := c.outputEnv(ctx, []string{"LC_ALL=C"}, "describe", "--tags",
		"--abbrev=0", "--match", c.match, "--exclude", exclude, "--end-of-options", c.ref)
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	if strings.Contains(diagnostic.String(), "No names found") ||
		strings.Contains(diagnostic.String(), "No tags can describe") ||
		strings.Contains(diagnostic.String(), "cannot describe") {
		return "", nil
	}
	return "", fmt.Errorf("describe %s: %s: %w", c.ref, diagnostic.String(), err)
}

// GitDiff returns a path-restricted diff between Base and Head.
func GitDiff(ctx context.Context, opts ...GitOption) (string, error) {
	c := newGitConfig(opts)
	if c.base == "" {
		return "", fmt.Errorf("GitDiff: base: %w", ErrMissingOption)
	}
	args := append([]string{"diff", "--end-of-options", c.base, c.head, "--"}, c.paths...)
	return c.output(ctx, args...)
}

// GitLog returns a path-restricted, one-line log without merge commits.
func GitLog(ctx context.Context, opts ...GitOption) (string, error) {
	c := newGitConfig(opts)
	if c.base == "" {
		return "", fmt.Errorf("GitLog: base: %w", ErrMissingOption)
	}
	args := []string{"log", "--oneline", "--no-merges"}
	if c.maxCount > 0 {
		args = append(args, "-n", strconv.Itoa(c.maxCount))
	}
	args = append(args, "--end-of-options", c.base+".."+c.head, "--")
	return c.output(ctx, append(args, c.paths...)...)
}

// GitShow returns Paths[0] at Ref.
func GitShow(ctx context.Context, opts ...GitOption) (string, error) {
	c := newGitConfig(opts)
	if c.ref == "" {
		return "", fmt.Errorf("GitShow: ref: %w", ErrMissingOption)
	}
	if len(c.paths) == 0 {
		return "", fmt.Errorf("GitShow: path: %w", ErrMissingOption)
	}
	return c.output(ctx, "show", "--end-of-options", c.ref+":"+c.paths[0])
}
