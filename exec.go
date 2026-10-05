// Package ci is documented in doc.go.
package ci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ErrMissingOption identifies a missing required functional option.
var ErrMissingOption = errors.New("missing required option")

type commandConfig struct {
	name   string
	args   []string
	dir    string
	env    []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// CommandOption configures RunCommand and CommandOutput.
type CommandOption func(*commandConfig)

// WithCommand sets the required executable and its arguments.
func WithCommand(name string, args ...string) CommandOption {
	return func(c *commandConfig) { c.name, c.args = name, args }
}

// WithCommandDir sets the command's working directory.
func WithCommandDir(dir string) CommandOption {
	return func(c *commandConfig) { c.dir = dir }
}

// WithCommandEnv appends environment entries to os.Environ().
func WithCommandEnv(kv ...string) CommandOption {
	return func(c *commandConfig) { c.env = append(c.env, kv...) }
}

// WithCommandStdin sets the input stream.
func WithCommandStdin(r io.Reader) CommandOption {
	return func(c *commandConfig) { c.stdin = r }
}

// WithCommandStdout sets the output stream, defaulting to os.Stdout.
func WithCommandStdout(w io.Writer) CommandOption {
	return func(c *commandConfig) { c.stdout = w }
}

// WithCommandStderr sets the error stream, defaulting to os.Stderr.
func WithCommandStderr(w io.Writer) CommandOption {
	return func(c *commandConfig) { c.stderr = w }
}

// RunCommand runs an executable with context cancellation and contextual errors.
func RunCommand(ctx context.Context, opts ...CommandOption) error {
	c := commandConfig{stdout: os.Stdout, stderr: os.Stderr}
	for _, opt := range opts {
		opt(&c)
	}
	if c.stdout == nil {
		c.stdout = os.Stdout
	}
	if c.stderr == nil {
		c.stderr = os.Stderr
	}
	if c.name == "" {
		return fmt.Errorf("RunCommand: command: %w", ErrMissingOption)
	}
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Dir, cmd.Env = c.dir, append(os.Environ(), c.env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", c.name, err)
	}
	return nil
}

// CommandOutput returns stdout; stderr goes to the configured writer.
func CommandOutput(ctx context.Context, opts ...CommandOption) (string, error) {
	var out bytes.Buffer
	opts = append(opts, WithCommandStdout(&out))
	if err := RunCommand(ctx, opts...); err != nil {
		return "", err
	}
	return out.String(), nil
}
