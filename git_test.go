// Package ci is documented in doc.go.
package ci

// cspell:ignore oneline NOSYSTEM
import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGitClone(t *testing.T) {
	ctx := context.Background()
	for _, opts := range [][]GitOption{nil, {WithGitURL("u")}} {
		if err := GitClone(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatalf("%v", err)
		}
	}
	log := filepath.Join(t.TempDir(), "args")
	t.Setenv("CI_GIT_ARGS", log)
	binary := testScript(t, "printf '%s\\n' \"$@\" >\"$CI_GIT_ARGS\"")
	opts := []GitOption{WithGitBinary(binary), WithGitURL("https://example.test/repo"),
		WithGitRef("v1"), WithGitDir("tree"), WithGitDepth(1), WithGitBare(true),
		WithGitFilter("blob:none"), WithGitStderr(io.Discard)}
	if err := GitClone(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "clone\n--branch\nv1\n--depth\n1\n--bare\n--fil" +
		"ter=blob:none\n--\nhttps://example.test/repo\n" +
		"tree\n"
	if string(data) != want {
		t.Fatalf("args %q", data)
	}
	if err := GitClone(
		ctx,
		WithGitBinary(binary),
		WithGitURL("u"),
		WithGitRef("r"),
	); err != nil {
		t.Fatal(err)
	}
	if err := GitClone(
		ctx,
		WithGitBinary("/missing/git"),
		WithGitURL("u"),
		WithGitRef("r"),
	); err == nil {
		t.Fatal("exit error")
	}
}
func TestGitChangedFiles(t *testing.T) {
	ctx := context.Background()
	if _, err := GitChangedFiles(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "args")
	t.Setenv("CI_GIT_ARGS", log)
	binary := testScript(
		t,
		"printf '%s\\n' \"$@\" >\"$CI_GIT_ARGS\"; printf 'sources/a/x\\0with space\\0'",
	)
	for _, merge := range []bool{false, true} {
		files, err := GitChangedFiles(
			ctx,
			WithGitBinary(binary),
			WithGitBase("origin/master"),
			WithGitHead("new"),
			WithGitMergeBase(merge),
			WithGitStderr(io.Discard),
		)
		if err != nil || !reflect.DeepEqual(files, []string{"sources/a/x", "with space"}) {
			t.Fatalf("%v %v", files, err)
		}
		data, _ := os.ReadFile(log)
		sep := ".."
		if merge {
			sep = "..."
		}
		wantArgs := "diff\n--name-only\n--no-renames\n-z\n--end-of-options\norigin/master" +
			sep + "new\n--\n"
		if string(data) != wantArgs {
			t.Fatalf("args %q, want %q", data, wantArgs)
		}
	}
	if _, err := GitChangedFiles(
		ctx,
		WithGitBase("x"),
		WithGitBinary("/missing/git"),
	); err == nil {
		t.Fatal("exit error")
	}
}
func TestGitPreviousTag(t *testing.T) {
	ctx := context.Background()
	for _, opts := range [][]GitOption{nil, {WithGitRef("v2")}} {
		if _, err := GitPreviousTag(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, script, want string
		fail               bool
	}{
		{"found", "printf 'app-v1\\n'", "app-v1", false},
		{"exit error", "exit 1", "", true},
		{"no names", "if [[ $1 == describe ]]; then echo 'No names " +
			"found' >&2; exit 128; fi", "", false},
		{"no tags", "if [[ $1 == describe ]]; then echo 'No tags can " +
			"describe' >&2; exit 128; fi", "", false},
		{"cannot describe", "echo 'cannot describe anything' >&2; exit 128", "", false},
		{"describe error", "if [[ $1 == describe ]]; then echo 'broken' >&" +
			"2; exit 128; fi", "", true},
		{"describe exit", "exit 128", "", true},
		{"forced C locale", "if [[ ${LC_ALL:-} == C ]]; then echo 'No names " +
			"found' >&2; else echo 'Имена не найдены' >&2; fi; exit 128", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LC_ALL", "ru_RU.UTF-8")
			got, err := GitPreviousTag(ctx, WithGitBinary(testScript(t, tt.script)),
				WithGitRef("app-v2"), WithGitMatch("app-v*"), WithGitStderr(io.Discard))
			if got != tt.want || (err != nil) != tt.fail {
				t.Fatalf("%q %v", got, err)
			}
		})
	}
	if _, err := GitPreviousTag(
		ctx,
		WithGitBinary("/missing/git"),
		WithGitRef("x"),
		WithGitMatch("*"),
	); err == nil {
		t.Fatal("start error")
	}
}
func gitTestRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := RunCommand(context.Background(), WithCommand("git", args...),
		WithCommandDir(dir), WithCommandEnv("GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.test", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+os.DevNull), WithCommandStdout(io.Discard),
		WithCommandStderr(io.Discard)); err != nil {
		t.Fatal(err)
	}
}
func TestGitChangedFilesRename(t *testing.T) {
	dir := t.TempDir()
	gitTestRun(t, dir, "init", "-q", "-b", "master")
	oldPath := "sources/a/patches/x.patch"
	newPath := "sources/b/patches/x.patch"
	for _, name := range []string{oldPath, newPath} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, oldPath)
	if err := os.WriteFile(file, []byte("patch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, dir, "add", ".")
	gitTestRun(t, dir, "commit", "-qm", "initial")
	gitTestRun(t, dir, "mv", oldPath, newPath)
	gitTestRun(t, dir, "commit", "-qm", "move patch")
	for _, merge := range []bool{false, true} {
		got, err := GitChangedFiles(context.Background(), WithGitDir(dir),
			WithGitBase("HEAD^"), WithGitMergeBase(merge))
		want := []string{oldPath, newPath}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("GitChangedFiles() = %v, want %v, error %v", got, want, err)
		}
	}
	owned := filepath.Join(t.TempDir(), "owned")
	if _, err := GitChangedFiles(context.Background(), WithGitDir(dir),
		WithGitBase("--output="+owned), WithGitStderr(io.Discard)); err == nil {
		t.Fatal("option-like base accepted as revision")
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("option-like base created %s: %v", owned, err)
	}
}
func TestGitPreviousTagRepository(t *testing.T) {
	for _, tt := range []struct {
		name, ref string
		commands  [][]string
		want      string
	}{
		{
			name: "same commit",
			commands: [][]string{
				{"commit", "--allow-empty", "-qm", "older release"},
				{"tag", "opencode-v1.15.12p0"},
				{"commit", "--allow-empty", "-qm", "current release"},
				{"tag", "opencode-v1.15.13p0"},
				{"tag", "opencode-v1.15.13p1"},
			},
			want: "opencode-v1.15.13p0",
		},
		{
			name: "only tag on root commit",
			commands: [][]string{
				{"commit", "--allow-empty", "-qm", "first release"},
				{"tag", "opencode-v1.15.13p1"},
			},
		},
		{
			name: "other application ignored",
			commands: [][]string{
				{"commit", "--allow-empty", "-qm", "other release"},
				{"tag", "pi-web-v1.202609.0p1"},
				{"commit", "--allow-empty", "-qm", "first application release"},
				{"tag", "opencode-v1.15.13p1"},
			},
		},
		{
			name: "ordinary chain",
			commands: [][]string{
				{"commit", "--allow-empty", "-qm", "previous release"},
				{"tag", "opencode-v1.15.13p0"},
				{"commit", "--allow-empty", "-qm", "current release"},
				{"tag", "opencode-v1.15.13p1"},
				{"tag", "pi-web-v1.202609.0p1"},
			},
			want: "opencode-v1.15.13p0",
		},
		{
			name: "full tag ref",
			ref:  "refs/tags/opencode-v1.15.13p1",
			commands: [][]string{
				{"commit", "--allow-empty", "-qm", "previous release"},
				{"tag", "opencode-v1.15.13p0"},
				{"commit", "--allow-empty", "-qm", "current release"},
				{"tag", "opencode-v1.15.13p1"},
			},
			want: "opencode-v1.15.13p0",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			gitTestRun(t, dir, "init", "-q", "-b", "master")
			for _, args := range tt.commands {
				gitTestRun(t, dir, args...)
			}
			got, err := GitPreviousTag(context.Background(), WithGitDir(dir),
				WithGitRef(cmp.Or(tt.ref, "opencode-v1.15.13p1")),
				WithGitMatch("opencode-v*"))
			if err != nil || got != tt.want {
				t.Fatalf("GitPreviousTag() = %q, want %q, error %v", got, tt.want, err)
			}
		})
	}
}
func TestGitDiff(t *testing.T) {
	ctx := context.Background()
	if _, err := GitDiff(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	binary := testScript(t, "printf '%s\\n' \"$@\"")
	got, err := GitDiff(
		ctx,
		WithGitBinary(binary),
		WithGitBase("v1"),
		WithGitHead("v2"),
		WithGitPaths("sources/a"),
	)
	if err != nil || got != "diff\n--end-of-options\nv1\nv2\n--\nsources/a\n" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := GitDiff(ctx, WithGitBinary("/missing/git"), WithGitBase("x")); err == nil {
		t.Fatal("exit error")
	}
}
func TestGitLog(t *testing.T) {
	ctx := context.Background()
	if _, err := GitLog(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	binary := testScript(t, "printf '%s\\n' \"$@\"")
	got, err := GitLog(
		ctx,
		WithGitBinary(binary),
		WithGitBase("v1"),
		WithGitHead("v2"),
		WithGitMaxCount(3),
		WithGitPaths("sources/a"),
	)
	want := "log\n--oneline\n--no-merges\n-n\n3\n--end-of-options\nv1..v2\n--\nsources/a\n"
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := GitLog(ctx, WithGitBinary(binary), WithGitBase("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := GitLog(ctx, WithGitBinary("/missing/git"), WithGitBase("x")); err == nil {
		t.Fatal("exit error")
	}
}
func TestGitShow(t *testing.T) {
	ctx := context.Background()
	for _, opts := range [][]GitOption{nil, {WithGitRef("v1")}} {
		if _, err := GitShow(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	got, err := GitShow(
		ctx,
		WithGitBinary(testScript(t, "printf '%s\\n' \"$@\"")),
		WithGitRef("v1"),
		WithGitPaths("sources/a/build.yaml"),
	)
	if err != nil || got != "show\n--end-of-options\nv1:sources/a/build.yaml\n" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := GitShow(
		ctx,
		WithGitBinary("/missing/git"),
		WithGitRef("x"),
		WithGitPaths("f"),
	); err == nil {
		t.Fatal("exit error")
	}
}
