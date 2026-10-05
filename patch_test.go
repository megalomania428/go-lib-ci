// Package ci is documented in doc.go.
package ci

// cspell:ignore stderrstderrstderr

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

func TestApplyPatches(t *testing.T) {
	ctx := context.Background()
	for _, opts := range [][]PatchOption{nil, {WithPatchTarget("tree")}} {
		if _, err := ApplyPatches(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	dir, last := t.TempDir(), t.TempDir()
	for _, name := range []string{"02.diff", "01.patch", "ignored.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("patch"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.diff"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(dir, "01.patch"),
		filepath.Join(dir, "link.patch"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(last, "03.diff"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	binary := testScript(t, "printf '%s\\n' \"$@\"; printf 'stderr' >&2")
	opts := []PatchOption{
		WithPatchTarget("tree"),
		WithPatchDirs(dir, last),
		WithPatchStrip(2),
		WithPatchBinary(binary),
		WithPatchStdout(&out),
		WithPatchStderr(&diagnostic),
	}
	files, err := ApplyPatches(ctx, opts...)
	want := []string{
		filepath.Join(dir, "01.patch"),
		filepath.Join(dir, "02.diff"),
		filepath.Join(last, "03.diff"),
	}
	if err != nil || !reflect.DeepEqual(files, want) {
		t.Fatalf("%v %v", files, err)
	}
	if !strings.Contains(out.String(), "-p2\n--batch\n--forward\n-i\n") ||
		diagnostic.String() != "stderrstderrstderr" {
		t.Fatalf("%s %s", &out, &diagnostic)
	}
	if files, err := ApplyPatches(
		ctx,
		WithPatchTarget("tree"),
		WithPatchDirs(t.TempDir()),
	); err != nil ||
		len(files) != 0 {
		t.Fatalf("empty: %v %v", files, err)
	}
	for _, extra := range []PatchOption{
		WithPatchDirs(filepath.Join(dir, "missing")), WithPatchBinary("/missing/patch"),
		WithPatchBinary(testScript(t, "exit 7")), WithPatchStdout(errorWriter{}),
	} {
		if _, err := ApplyPatches(ctx, append(opts, extra)...); err == nil {
			t.Fatal("expected failure")
		}
	}
	if _, err := ApplyPatches(ctx, append(opts, WithPatchStdout(nil),
		WithPatchStderr(nil))...); err != nil {
		t.Fatalf("nil output writer must use the default instead of panicking: %v", err)
	}
	original := patchAbsolutePath
	patchAbsolutePath = func(string) (string, error) {
		return "", errors.New(
			"abs failure",
		)
	}
	t.Cleanup(func() { patchAbsolutePath = original })
	if _, err := ApplyPatches(ctx, opts...); err == nil {
		t.Fatal("path error")
	}
}
func TestApplyPatchesReal(t *testing.T) {
	root, patches := t.TempDir(), t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "file"),
		[]byte("old\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	data := "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n"
	if err := os.WriteFile(
		filepath.Join(patches, "01.patch"),
		[]byte(data),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	opts := []PatchOption{
		WithPatchTarget(root),
		WithPatchDirs(patches),
		WithPatchStdout(io.Discard),
		WithPatchStderr(io.Discard),
	}
	if _, err := ApplyPatches(context.Background(), opts...); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "file"))
	if string(got) != "new\n" {
		t.Fatal(string(got))
	}
	if _, err := ApplyPatches(context.Background(), opts...); err == nil {
		t.Fatal("reapplication must fail")
	}
}
