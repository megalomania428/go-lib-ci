// Package ci is documented in doc.go.
package ci

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// patchAbsolutePath allows deterministic path-resolution failure tests.
var patchAbsolutePath = filepath.Abs

type patchConfig struct {
	target, binary string
	dirs           []string
	strip          int
	stdout, stderr io.Writer
}

// PatchOption configures ApplyPatches.
type PatchOption func(*patchConfig)

// WithPatchTarget sets the required upstream working tree.
func WithPatchTarget(dir string) PatchOption {
	return func(c *patchConfig) { c.target = dir }
}

// WithPatchDirs sets ordered directories of patches.
func WithPatchDirs(dirs ...string) PatchOption {
	return func(c *patchConfig) { c.dirs = dirs }
}

// WithPatchStrip sets the number of path components to strip, defaulting to one.
func WithPatchStrip(n int) PatchOption { return func(c *patchConfig) { c.strip = n } }

// WithPatchBinary overrides the patch executable.
func WithPatchBinary(path string) PatchOption {
	return func(c *patchConfig) { c.binary = path }
}

// WithPatchStdout sets the informational and command output stream.
func WithPatchStdout(w io.Writer) PatchOption {
	return func(c *patchConfig) { c.stdout = w }
}

// WithPatchStderr sets the error stream.
func WithPatchStderr(w io.Writer) PatchOption {
	return func(c *patchConfig) { c.stderr = w }
}

// ApplyPatches applies regular .diff and .patch files sorted within each directory.
func ApplyPatches(ctx context.Context, opts ...PatchOption) ([]string, error) {
	c := patchConfig{binary: "patch", strip: 1, stdout: os.Stdout, stderr: os.Stderr}
	for _, opt := range opts {
		opt(&c)
	}
	if c.stdout == nil {
		c.stdout = os.Stdout
	}
	if c.target == "" {
		return nil, fmt.Errorf("ApplyPatches: target: %w", ErrMissingOption)
	}
	if len(c.dirs) == 0 {
		return nil, fmt.Errorf("ApplyPatches: dirs: %w", ErrMissingOption)
	}
	var applied []string
	for _, dir := range c.dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return applied, fmt.Errorf("read patch directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			ext := filepath.Ext(entry.Name())
			if !entry.Type().IsRegular() || (ext != ".diff" && ext != ".patch") {
				continue
			}
			file, err := patchAbsolutePath(filepath.Join(dir, entry.Name()))
			if err != nil {
				return applied, fmt.Errorf("resolve patch path: %w", err)
			}
			if _, err := fmt.Fprintf(c.stdout, "applying %s\n", file); err != nil {
				return applied, fmt.Errorf("log patch: %w", err)
			}
			args := []string{"-d", c.target, "-p" + strconv.Itoa(c.strip),
				"--batch", "--forward", "-i", file}
			if err := RunCommand(ctx, WithCommand(c.binary, args...),
				WithCommandStdout(c.stdout), WithCommandStderr(c.stderr)); err != nil {
				return applied, fmt.Errorf("apply %s: %w", file, err)
			}
			applied = append(applied, file)
		}
	}
	return applied, nil
}
