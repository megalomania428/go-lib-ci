// Package ci is documented in doc.go.
package ci

// cspell:ignore pipefail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testScript(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "command")
	if err := os.WriteFile(
		file,
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body+"\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestRunCommand(t *testing.T) {
	ctx := context.Background()
	if err := RunCommand(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatalf("missing: %v", err)
	}
	dir := t.TempDir()
	var out, diagnostic bytes.Buffer
	script := testScript(
		t,
		"printf '%s:%s:%s:' \"$PWD\" \"$CI_TEST_ONE\" "+
			"\"$CI_TEST_TWO\"; cat; printf 'warning' >&2",
	)
	err := RunCommand(
		ctx,
		WithCommand(script),
		WithCommandDir(dir),
		WithCommandEnv("CI_TEST_ONE=one"),
		WithCommandEnv("CI_TEST_TWO=two"),
		WithCommandStdin(
			strings.NewReader("input"),
		),
		WithCommandStdout(&out),
		WithCommandStderr(&diagnostic),
	)
	if err != nil || out.String() != dir+":one:two:input" ||
		diagnostic.String() != "warning" {
		t.Fatalf("command: %q %q %v", out.String(), diagnostic.String(), err)
	}
	for _, opts := range [][]CommandOption{
		{WithCommand(testScript(t, "exit 7"), "arg")},
		{WithCommand(filepath.Join(dir, "missing"))},
		{WithCommand("bash", "-c", "echo x"), WithCommandDir(filepath.Join(dir, "missing"))},
		{WithCommand("bash", "-c", "echo x"), WithCommandStdout(errorWriter{})},
		{WithCommand("bash", "-c", "cat"), WithCommandStdin(errorReader{})},
	} {
		if err := RunCommand(ctx, opts...); err == nil {
			t.Fatal("expected command error")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := RunCommand(canceled, WithCommand("bash", "-c", "true")); err == nil {
		t.Fatal("expected cancellation")
	}
	if err := RunCommand(ctx, WithCommand("bash", "-c", "true")); err != nil {
		t.Fatal(err)
	}
}
func TestCommandWriterDefaults(t *testing.T) {
	err := RunCommand(context.Background(), WithCommand("bash", "-c", "true"),
		WithCommandStdout(nil), WithCommandStderr(nil))
	if err != nil {
		t.Fatal(err)
	}
	binary := testScript(t, "exit 7")
	err = RunCommand(context.Background(), WithCommand(binary,
		"https://user:secret-token@example.com/private.git", "--password=another-secret"))
	if err == nil || !strings.Contains(err.Error(), "exit status 7") ||
		!strings.Contains(err.Error(), binary) {
		t.Fatalf("command or exit status missing: %v", err)
	}
	secrets := []string{"user", "secret-token", "another-secret", "private.git"}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("command argument leaked: %v", err)
		}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("exit error lost: %v", err)
	}
}
func TestCommandOutput(t *testing.T) {
	var ignored bytes.Buffer
	out, err := CommandOutput(
		context.Background(),
		WithCommand("bash", "-c", "printf hello"),
		WithCommandStdout(&ignored),
		WithCommandStderr(io.Discard),
	)
	if err != nil || out != "hello" || ignored.Len() != 0 {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := CommandOutput(
		context.Background(),
		WithCommand("bash", "-c", "exit 2"),
	); err == nil {
		t.Fatal("expected failure")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failure") }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }
func withDeletedCWD(t *testing.T, fn func()) {
	t.Helper()
	old, err := os.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := old.Chdir(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	fn()
}
