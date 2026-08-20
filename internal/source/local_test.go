package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestLocal builds a small tree:
//
//	root/
//	  a.jpg
//	  notes.txt
//	  .hidden
//	  sub/
//	    b.png
func newTestLocal(t *testing.T) (*Local, string) {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	mustWrite(t, filepath.Join(root, "a.jpg"), "jpeg-bytes")
	mustWrite(t, filepath.Join(root, "notes.txt"), "hello")
	mustWrite(t, filepath.Join(root, ".hidden"), "secret")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "b.png"), "png-bytes")

	l, err := NewLocal(root, "")
	if err != nil {
		t.Fatal(err)
	}
	return l, root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// within reports whether abs is root or sits underneath it. filepath.Rel is
// used rather than a string prefix so that path boundaries are respected:
// "/data-other" must not count as being inside "/data".
func within(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func TestLocalResolveContainment(t *testing.T) {
	l, root := newTestLocal(t)

	for _, ok := range []string{"", "a.jpg", "sub", "sub/b.png", "/a.jpg", "sub/../a.jpg"} {
		abs, err := l.resolve(ok)
		if err != nil {
			t.Errorf("resolve(%q) unexpected error: %v", ok, err)
			continue
		}
		if !within(root, abs) {
			t.Errorf("resolve(%q) = %q, escaped root %q", ok, abs, root)
		}
	}

	// Traversal attempts must never reach outside the root. After cleaning
	// these become ordinary relative paths that simply do not exist.
	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"../" + filepath.Base(root),
		"sub/../../../../etc/passwd",
	} {
		if _, err := l.resolve(bad); err == nil {
			t.Errorf("resolve(%q) succeeded, expected failure", bad)
		}
	}
}

func TestLocalRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	l, root := newTestLocal(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWrite(t, secret, "do not serve me")

	if err := os.Symlink(secret, filepath.Join(root, "escape")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Resolving the link itself must be refused...
	if _, err := l.resolve("escape"); !errors.Is(err, ErrForbidden) {
		t.Errorf("resolve(escape) error = %v, want ErrForbidden", err)
	}

	// ...and it must not appear in listings either.
	entries, err := l.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "escape" {
			t.Error("escaping symlink was listed")
		}
	}
}

func TestLocalAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	l, root := newTestLocal(t)

	if err := os.Symlink(filepath.Join(root, "a.jpg"), filepath.Join(root, "alias.jpg")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := l.resolve("alias.jpg"); err != nil {
		t.Errorf("internal symlink should resolve, got %v", err)
	}
}

func TestLocalListHidesDotfiles(t *testing.T) {
	l, _ := newTestLocal(t)

	entries, err := l.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // sub, a.jpg, notes.txt — .hidden excluded
		t.Fatalf("got %d entries, want 3: %v", len(entries), names(entries))
	}
	for _, e := range entries {
		if e.Name == ".hidden" {
			t.Error("dotfile was listed")
		}
	}

	Sort(entries, SortName, false)
	if !entries[0].IsDir || entries[0].Name != "sub" {
		t.Errorf("folders must sort first, got %v", names(entries))
	}
}

func TestLocalStat(t *testing.T) {
	l, _ := newTestLocal(t)
	ctx := context.Background()

	root, err := l.Stat(ctx, "")
	if err != nil || !root.IsDir {
		t.Fatalf("root stat = %+v, %v", root, err)
	}

	file, err := l.Stat(ctx, "sub/b.png")
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "b.png" || file.Path != "sub/b.png" || file.IsDir {
		t.Errorf("unexpected entry %+v", file)
	}
	if file.Kind != KindImage || file.CacheTag == "" {
		t.Errorf("entry not described: %+v", file)
	}

	if _, err := l.Stat(ctx, "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file error = %v, want ErrNotFound", err)
	}
}

func TestLocalOpenAndSeek(t *testing.T) {
	l, _ := newTestLocal(t)

	f, err := l.Open(context.Background(), "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if f.Size() != int64(len("hello")) {
		t.Errorf("Size = %d, want 5", f.Size())
	}
	buf := make([]byte, 3)
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hel" {
		t.Errorf("read %q, want hel", buf)
	}
}

func TestLocalOpenRejectsDirectory(t *testing.T) {
	l, _ := newTestLocal(t)
	if _, err := l.Open(context.Background(), "sub"); err == nil {
		t.Error("opening a directory should fail")
	}
}

func TestLocalWalk(t *testing.T) {
	l, _ := newTestLocal(t)

	var seen []string
	err := l.Walk(context.Background(), "", func(e Entry) error {
		if e.IsDir {
			t.Errorf("Walk reported a directory: %q", e.Path)
		}
		seen = append(seen, e.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"a.jpg": true, "notes.txt": true, "sub/b.png": true}
	if len(seen) != len(want) {
		t.Fatalf("walked %v, want %v", seen, want)
	}
	for _, p := range seen {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestLocalWalkStopsOnError(t *testing.T) {
	l, _ := newTestLocal(t)

	stop := errors.New("stop")
	count := 0
	err := l.Walk(context.Background(), "", func(Entry) error {
		count++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("Walk error = %v, want the sentinel", err)
	}
	if count != 1 {
		t.Errorf("callback ran %d times after asking to stop", count)
	}
}

func TestNewLocalRejectsBadRoot(t *testing.T) {
	if _, err := NewLocal(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Error("expected an error for a missing root")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	mustWrite(t, file, "x")
	if _, err := NewLocal(file, ""); err == nil {
		t.Error("expected an error when the root is a file")
	}
}
