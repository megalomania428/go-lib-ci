// Package ci is documented in doc.go.
package ci

// cspell:ignore LZMA lzma

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type archiveConfig struct {
	source, output, binary string
	stdout, stderr         io.Writer
}

// ArchiveOption configures ArchiveTarXz and Archive7z.
type ArchiveOption func(*archiveConfig)

// WithArchiveSource sets the required file or directory to pack.
func WithArchiveSource(path string) ArchiveOption {
	return func(c *archiveConfig) { c.source = path }
}

// WithArchiveOutput sets the required archive destination.
func WithArchiveOutput(path string) ArchiveOption {
	return func(c *archiveConfig) { c.output = path }
}

// WithArchiveBinary overrides tar or 7z.
func WithArchiveBinary(path string) ArchiveOption {
	return func(c *archiveConfig) { c.binary = path }
}

// WithArchiveStdout sets command stdout.
func WithArchiveStdout(w io.Writer) ArchiveOption {
	return func(c *archiveConfig) { c.stdout = w }
}

// WithArchiveStderr sets command stderr.
func WithArchiveStderr(w io.Writer) ArchiveOption {
	return func(c *archiveConfig) { c.stderr = w }
}
func newArchiveConfig(binary string, opts []ArchiveOption) (*archiveConfig, error) {
	c := &archiveConfig{binary: binary, stdout: os.Stdout, stderr: os.Stderr}
	for _, opt := range opts {
		opt(c)
	}
	if c.source == "" {
		return nil, fmt.Errorf("archive: source: %w", ErrMissingOption)
	}
	if c.output == "" {
		return nil, fmt.Errorf("archive: output: %w", ErrMissingOption)
	}
	var err error
	c.source, err = filepath.Abs(c.source)
	if err != nil {
		return nil, fmt.Errorf("resolve source: %w", err)
	}
	c.output, err = filepath.Abs(c.output)
	if err != nil {
		return nil, fmt.Errorf("resolve output: %w", err)
	}
	realSource := resolveSymlinks(c.source)
	sourceEntry := filepath.Join(resolveSymlinks(filepath.Dir(c.source)),
		filepath.Base(c.source))
	realOutput := filepath.Join(resolveSymlinks(filepath.Dir(c.output)),
		filepath.Base(c.output))
	if realSource == realOutput || sourceEntry == realOutput {
		return nil, errors.New("archive: source equals output")
	}
	prefix := strings.TrimSuffix(realSource, string(os.PathSeparator)) +
		string(os.PathSeparator)
	if binary == "tar" && strings.HasPrefix(realOutput, prefix) {
		return nil, errors.New("archive: tar output is inside source")
	}
	if err := os.Remove(c.output); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove old archive: %w", err)
	}
	return c, nil
}

func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// ArchiveTarXz packs Source by base name with tar and xz -T0 -9e.
func ArchiveTarXz(ctx context.Context, opts ...ArchiveOption) error {
	c, err := newArchiveConfig("tar", opts)
	if err != nil {
		return err
	}
	return RunCommand(ctx, WithCommand(c.binary, "-C", filepath.Dir(c.source),
		"--use-compress-program=xz -T0 -9e", "-cf", c.output, "--", filepath.Base(c.source)),
		WithCommandStdout(c.stdout), WithCommandStderr(c.stderr))
}

// Archive7z packs Source by base name with 7z and maximum LZMA2 compression.
func Archive7z(ctx context.Context, opts ...ArchiveOption) error {
	c, err := newArchiveConfig("7z", opts)
	if err != nil {
		return err
	}
	return RunCommand(ctx, WithCommand(c.binary, "a", "-t7z", "-m0=lzma2", "-mx=9",
		c.output, "--", filepath.Base(c.source)), WithCommandDir(filepath.Dir(c.source)),
		WithCommandStdout(c.stdout), WithCommandStderr(c.stderr))
}
