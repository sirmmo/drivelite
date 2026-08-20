package source

import (
	"strings"
	"testing"
)

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
		// Backslashes are normalised, so a Windows-style path cannot smuggle
		// a separator past the checks below.
		`sub\b.png`: "sub/b.png",
		// "...." is an ordinary (if odd) directory name, not a traversal.
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
		"./../.././x",
		`..\..\windows\system32`,
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

func TestPathHelpers(t *testing.T) {
	if got := Parent("a/b/c.txt"); got != "a/b" {
		t.Errorf("Parent = %q, want a/b", got)
	}
	if got := Parent("c.txt"); got != "" {
		t.Errorf("Parent of a top-level file = %q, want empty", got)
	}
	if got := Base("a/b/c.txt"); got != "c.txt" {
		t.Errorf("Base = %q, want c.txt", got)
	}
	if got := Base("c.txt"); got != "c.txt" {
		t.Errorf("Base = %q, want c.txt", got)
	}
	if got := Join("", "a"); got != "a" {
		t.Errorf("Join with empty dir = %q, want a", got)
	}
	if got := Join("a/b", "c"); got != "a/b/c" {
		t.Errorf("Join = %q, want a/b/c", got)
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
		{"main.go", KindText, false, false},
		{"archive.zip", KindOther, false, false},
	}
	for _, c := range cases {
		e := Describe(Entry{Name: c.name, Path: c.name})
		if e.Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q (mime %q)", c.name, e.Kind, c.kind, e.MimeType)
		}
		if e.Previewable != c.prev {
			t.Errorf("%s: Previewable = %v, want %v", c.name, e.Previewable, c.prev)
		}
		if e.Playable != c.play {
			t.Errorf("%s: Playable = %v, want %v", c.name, e.Playable, c.play)
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

// ".ts" is ambiguous (MPEG transport stream vs TypeScript). drivelite keeps
// the media meaning; this test exists so the choice is not reversed by
// accident.
func TestTypeScriptExtensionStaysVideo(t *testing.T) {
	if got := MimeOf("app.ts"); got != "video/mp2t" {
		t.Errorf("MimeOf(app.ts) = %q, want video/mp2t", got)
	}
	if got := MimeOf("app.tsx"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("MimeOf(app.tsx) = %q, want text/plain", got)
	}
}

func TestSortFoldersAlwaysLead(t *testing.T) {
	entries := []Entry{
		{Name: "zeta.txt", Size: 10, ModUnix: 300},
		{Name: "alpha", IsDir: true, ModUnix: 100},
		{Name: "beta.txt", Size: 50, ModUnix: 200},
		{Name: "omega", IsDir: true, ModUnix: 400},
	}

	for _, desc := range []bool{false, true} {
		for _, mode := range []SortMode{SortName, SortSize, SortDate} {
			got := append([]Entry(nil), entries...)
			Sort(got, mode, desc)
			if !got[0].IsDir || !got[1].IsDir {
				t.Errorf("mode=%s desc=%v: folders did not lead: %v", mode, desc, names(got))
			}
		}
	}

	byName := append([]Entry(nil), entries...)
	Sort(byName, SortName, false)
	if byName[0].Name != "alpha" || byName[2].Name != "beta.txt" {
		t.Errorf("ascending name sort = %v", names(byName))
	}

	bySize := append([]Entry(nil), entries...)
	Sort(bySize, SortSize, true)
	if bySize[2].Name != "beta.txt" {
		t.Errorf("descending size sort should lead with the largest file: %v", names(bySize))
	}
}

func names(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func TestHidden(t *testing.T) {
	for _, n := range []string{".git", ".env", ".DS_Store"} {
		if !Hidden(n) {
			t.Errorf("Hidden(%q) = false", n)
		}
	}
	for _, n := range []string{"photo.jpg", "a.b.c"} {
		if Hidden(n) {
			t.Errorf("Hidden(%q) = true", n)
		}
	}
}
