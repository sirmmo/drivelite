package vault

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestVault builds a small tree:
//
//	root/
//	  a.jpg
//	  notes.txt
//	  sub/
//	    b.png
//	  .hidden
func newTestVault(t *testing.T) (*Vault, string) {
	t.Helper()
	root := t.TempDir()
	// Resolve symlinks: on macOS t.TempDir() sits under /var -> /private/var.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	root = resolved

	mustWrite(t, filepath.Join(root, "a.jpg"), "jpeg-bytes")
	mustWrite(t, filepath.Join(root, "notes.txt"), "hello")
	mustWrite(t, filepath.Join(root, ".hidden"), "secret")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "b.png"), "png-bytes")

	return New(root), root
}

// within reports whether abs is root or sits underneath it. filepath.Rel is
// used rather than a string prefix so that path boundaries are respected:
// "/data-other" must not count as being inside "/data".
func within(t *testing.T, root, abs string) bool {
	t.Helper()
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"/":                  "",
		".":                  "",
		"a.jpg":              "a.jpg",
		"/a.jpg":             "a.jpg",
		"sub/b.png":          "sub/b.png",
		"../../etc/passwd":   "etc/passwd",
		"/../../etc/passwd":  "etc/passwd",
		"sub/../a.jpg":       "a.jpg",
		"sub/../../../a.jpg": "a.jpg",
		"./sub//b.png":       "sub/b.png",
		// "...." is an ordinary (if odd) directory name, not a traversal:
		// it is kept, while the repeated separators are collapsed.
		"....//....//x": "..../..../x",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// The property that actually matters: whatever the input, the cleaned result
// never contains a ".." segment that could climb out of the root.
func TestCleanNeverYieldsParentSegment(t *testing.T) {
	inputs := []string{
		"../../etc/passwd",
		"/../../etc/passwd",
		"a/../../../b",
		"....//....//x",
		"..",
		"../",
		"sub/..%2f../x",
		"./../.././x",
		strings.Repeat("../", 40) + "etc/passwd",
	}
	for _, in := range inputs {
		got := Clean(in)
		if strings.HasPrefix(got, "/") {
			t.Errorf("Clean(%q) = %q, must be relative", in, got)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == ".." {
				t.Errorf("Clean(%q) = %q, contains a %q segment", in, got, "..")
			}
		}
	}
}

func TestResolveContainment(t *testing.T) {
	v, root := newTestVault(t)

	// Paths that must resolve successfully.
	for _, ok := range []string{"", "a.jpg", "sub", "sub/b.png", "/a.jpg", "sub/../a.jpg"} {
		abs, err := v.Resolve(ok)
		if err != nil {
			t.Errorf("Resolve(%q) unexpected error: %v", ok, err)
			continue
		}
		if !within(t, root, abs) {
			t.Errorf("Resolve(%q) = %q, escaped root %q", ok, abs, root)
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
		if _, err := v.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) succeeded, expected failure", bad)
		}
	}
}

func TestResolveRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	v, root := newTestVault(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWrite(t, secret, "do not serve me")

	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Resolving the link itself must be refused...
	if _, err := v.Resolve("escape"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Resolve(escape) error = %v, want ErrForbidden", err)
	}

	// ...and it must not appear in listings either.
	entries, err := v.List("", SortName, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "escape" {
			t.Error("escaping symlink was listed")
		}
	}
}

func TestResolveAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	v, root := newTestVault(t)

	link := filepath.Join(root, "alias.jpg")
	if err := os.Symlink(filepath.Join(root, "a.jpg"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := v.Resolve("alias.jpg"); err != nil {
		t.Errorf("internal symlink should resolve, got %v", err)
	}
}

func TestListHidesDotfilesAndSortsFoldersFirst(t *testing.T) {
	v, _ := newTestVault(t)

	entries, err := v.List("", SortName, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // sub, a.jpg, notes.txt — .hidden excluded
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if !entries[0].IsDir || entries[0].Name != "sub" {
		t.Errorf("folders must sort first, got %q", entries[0].Name)
	}
	for _, e := range entries {
		if e.Name == ".hidden" {
			t.Error("dotfile was listed")
		}
	}
}

func TestListSortModes(t *testing.T) {
	v, root := newTestVault(t)
	mustWrite(t, filepath.Join(root, "big.jpg"), "0123456789")

	bySize, err := v.List("", SortSize, true) // descending
	if err != nil {
		t.Fatal(err)
	}
	// Folders still lead regardless of direction.
	if !bySize[0].IsDir {
		t.Errorf("descending sort must still put folders first, got %q", bySize[0].Name)
	}
	var files []Entry
	for _, e := range bySize {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	if files[0].Name != "big.jpg" {
		t.Errorf("largest file should lead descending size sort, got %q", files[0].Name)
	}
}

func TestEntryClassification(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		prev bool
		play bool
	}{
		{"photo.JPG", KindImage, true, false},
		{"photo.heic", KindImage, false, false},
		{"clip.mp4", KindVideo, false, true},
		{"clip.MTS", KindVideo, false, false}, // AVCHD: no browser plays it
		{"song.mp3", KindAudio, false, true},
		{"doc.pdf", KindPDF, false, false},
		{"readme.txt", KindText, false, false},
		{"archive.zip", KindOther, false, false},
	}
	for _, c := range cases {
		mimeType := MimeOf(c.name)
		if got := kindOf(mimeType, c.name); got != c.kind {
			t.Errorf("%s: kind = %q, want %q (mime %q)", c.name, got, c.kind, mimeType)
		}
		if got := Decodable(c.name); got != c.prev {
			t.Errorf("%s: Decodable = %v, want %v", c.name, got, c.prev)
		}
		if got := playable(c.name, mimeType); got != c.play {
			t.Errorf("%s: playable = %v, want %v", c.name, got, c.play)
		}
	}
}

// MIME types must come from the built-in table, because a minimal container
// image has no /etc/mime.types and Go's own table omits .mp4 entirely.
func TestMimeOfIsSelfContained(t *testing.T) {
	cases := map[string]string{
		"a.mp4":  "video/mp4",
		"a.MTS":  "video/mp2t",
		"a.jpg":  "image/jpeg",
		"a.png":  "image/png",
		"a.webm": "video/webm",
		"a.mp3":  "audio/mpeg",
		"a.wat":  "application/octet-stream",
	}
	for name, want := range cases {
		if got := MimeOf(name); got != want {
			t.Errorf("MimeOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRelOf(t *testing.T) {
	v, root := newTestVault(t)
	got, err := v.RelOf(filepath.Join(root, "sub", "b.png"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "sub/b.png" {
		t.Errorf("RelOf = %q, want sub/b.png", got)
	}
	if _, err := v.RelOf(filepath.Join(filepath.Dir(root), "elsewhere")); !errors.Is(err, ErrForbidden) {
		t.Errorf("RelOf outside root should be forbidden, got %v", err)
	}
}

func TestDirSizeLimit(t *testing.T) {
	v, root := newTestVault(t)

	size, files, over := v.DirSize(root, 0)
	if over {
		t.Error("no limit should never report exceeded")
	}
	if files != 3 { // a.jpg, notes.txt, sub/b.png (.hidden skipped)
		t.Errorf("counted %d files, want 3", files)
	}
	if size == 0 {
		t.Error("size should be non-zero")
	}

	if _, _, over := v.DirSize(root, 1); !over {
		t.Error("tiny limit should report exceeded")
	}
}
