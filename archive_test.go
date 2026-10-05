// Package ci is documented in doc.go.
package ci

// cspell:ignore LZMA lzma

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArchiveTarXz(t *testing.T) { testArchive(t, ArchiveTarXz, false) }
func TestArchive7z(t *testing.T)    { testArchive(t, Archive7z, true) }

func testArchive(
	t *testing.T,
	pack func(context.Context, ...ArchiveOption) error,
	seven bool,
) {
	t.Helper()
	ctx := context.Background()
	for _, opts := range [][]ArchiveOption{nil, {WithArchiveSource("source")}} {
		if err := pack(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	source, output := filepath.Join(dir, "-source"), filepath.Join(dir, "archive")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("old archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	script := testScript(t, "printf '%s\\n' \"$@\"; printf 'warning' >&2")
	opts := []ArchiveOption{WithArchiveSource(source), WithArchiveOutput(output),
		WithArchiveBinary(script), WithArchiveStdout(&out), WithArchiveStderr(&diagnostic)}
	if err := pack(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("old archive wasn't removed: %v", err)
	}
	want := "-C\n" + dir + "\n--use-compress-program=xz -T0 -9e\n-cf\n" +
		output + "\n--\n-source\n"
	if seven {
		want = "a\n-t7z\n-m0=lzma2\n-mx=9\n" + output + "\n--\n-source\n"
	}
	if out.String() != want || diagnostic.String() != "warning" {
		t.Fatalf("%q %q", &out, &diagnostic)
	}
	if err := pack(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	if err := pack(
		ctx,
		append(opts, WithArchiveBinary(testScript(t, "exit 3")))...,
	); err == nil {
		t.Fatal("exit error")
	}
	if err := pack(ctx, append(opts, WithArchiveOutput(source))...); err == nil {
		t.Fatal("same path")
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "child"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pack(ctx, opts...); err == nil {
		t.Fatal("remove error")
	}
	withDeletedCWD(t, func() {
		if err := pack(
			ctx,
			WithArchiveSource("relative"),
			WithArchiveOutput(output),
		); err == nil ||
			!strings.Contains(err.Error(), "resolve source") {
			t.Fatalf("%v", err)
		}
		if err := pack(
			ctx,
			WithArchiveSource(source),
			WithArchiveOutput("relative"),
		); err == nil ||
			!strings.Contains(err.Error(), "resolve output") {
			t.Fatalf("%v", err)
		}
	})
}
func TestArchiveTarXzReal(t *testing.T) {
	dir := t.TempDir()
	file, output := filepath.Join(dir, "-binary"), filepath.Join(dir, "binary.txz")
	if err := os.WriteFile(file, []byte("small archive payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveTarXz(
		context.Background(),
		WithArchiveSource(file),
		WithArchiveOutput(output),
		WithArchiveStdout(io.Discard),
	); err != nil {
		t.Fatal(err)
	}
	list, err := CommandOutput(context.Background(), WithCommand("tar", "-tf", output))
	if err != nil || list != "-binary\n" {
		t.Fatalf("%q %v", list, err)
	}
}
func TestArchive7zReal(t *testing.T) {
	dir := t.TempDir()
	file, output := filepath.Join(dir, "-binary"), filepath.Join(dir, "binary.7z")
	const payload = "small archive payload"
	if err := os.WriteFile(file, []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	type args struct {
		ctx  context.Context
		opts []ArchiveOption
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "leading dash source",
			args: args{ctx: context.Background(), opts: []ArchiveOption{
				WithArchiveSource(file), WithArchiveOutput(output),
				WithArchiveStdout(io.Discard),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Archive7z(tt.args.ctx, tt.args.opts...); (err != nil) != tt.wantErr {
				t.Fatalf("Archive7z() error = %v, wantErr %v", err, tt.wantErr)
			}
			got, err := CommandOutput(tt.args.ctx,
				WithCommand("7z", "x", "-so", output, "--", "-binary"))
			if err != nil || got != payload {
				t.Fatalf("archive payload = %q, error = %v", got, err)
			}
		})
	}
}

func Test_newArchiveConfig(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(source, "pkg.txz")
	rootOutput := filepath.Join(dir, "root.txz")
	for _, path := range []string{inside, rootOutput} {
		if err := os.WriteFile(path, []byte("old archive"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sibling := filepath.Join(dir, "pkg.txz")
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(realDir, "file")
	if err := os.WriteFile(realFile, []byte("old archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	aliasSource := filepath.Join(dir, "alias-pkg")
	if err := os.Symlink(source, aliasSource); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing", "src")
	sevenOutput := filepath.Join(source, "pkg.7z")
	type args struct {
		binary string
		opts   []ArchiveOption
	}
	tests := []struct {
		name    string
		args    args
		want    *archiveConfig
		wantErr bool
		errText string
	}{
		{
			name: "tar rejects nested output before removing it",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(source), WithArchiveOutput(inside),
				WithArchiveBinary("custom-tar"),
			}},
			wantErr: true,
		},
		{
			name: "tar rejects output within root source",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(string(os.PathSeparator)), WithArchiveOutput(rootOutput),
			}},
			wantErr: true,
		},
		{
			name: "rejects source equal to output through symlink",
			args: args{binary: "7z", opts: []ArchiveOption{
				WithArchiveSource(filepath.Join(alias, "file")),
				WithArchiveOutput(realFile),
			}},
			wantErr: true,
			errText: "source equals output",
		},
		{
			name: "rejects output equal to symlinked source",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(aliasSource), WithArchiveOutput(aliasSource),
			}},
			wantErr: true,
			errText: "source equals output",
		},
		{
			name: "tar rejects output inside source through symlink",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(aliasSource), WithArchiveOutput(inside),
			}},
			wantErr: true,
		},
		{
			name: "tar accepts missing source and output directory",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(missing),
				WithArchiveOutput(filepath.Join(dir, "missing", "out.txz")),
			}},
			want: &archiveConfig{source: missing,
				output: filepath.Join(dir, "missing", "out.txz"), binary: "tar",
				stdout: os.Stdout, stderr: os.Stderr},
		},
		{
			name: "tar accepts sibling sharing source prefix",
			args: args{binary: "tar", opts: []ArchiveOption{
				WithArchiveSource(source), WithArchiveOutput(sibling),
			}},
			want: &archiveConfig{source: source, output: sibling, binary: "tar",
				stdout: os.Stdout, stderr: os.Stderr},
		},
		{
			name: "7z behavior is unchanged",
			args: args{binary: "7z", opts: []ArchiveOption{
				WithArchiveSource(source), WithArchiveOutput(sevenOutput),
			}},
			want: &archiveConfig{source: source, output: sevenOutput, binary: "7z",
				stdout: os.Stdout, stderr: os.Stderr},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newArchiveConfig(tt.args.binary, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newArchiveConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errText := tt.errText
				if errText == "" {
					errText = "tar output is inside source"
				}
				if !strings.Contains(err.Error(), errText) {
					t.Fatalf("unexpected rejection: %v", err)
				}
				for _, path := range []string{inside, rootOutput, realFile} {
					data, readErr := os.ReadFile(path)
					if readErr != nil || string(data) != "old archive" {
						t.Fatalf("existing archive modified: %q %v", data, readErr)
					}
				}
				if _, statErr := os.Lstat(aliasSource); statErr != nil {
					t.Fatalf("source symlink removed: %v", statErr)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newArchiveConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}
