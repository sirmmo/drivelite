package httpd

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drivelite/internal/auth"
	"drivelite/internal/config"
	"drivelite/internal/thumbs"
	"drivelite/internal/vault"
)

const testPassword = "s3cret"

// newTestServer builds a server over a temporary tree containing a real JPEG,
// a text file, an HTML file and a subfolder.
func newTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()

	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	cacheDir := t.TempDir()

	// A real JPEG, so thumbnail generation has something to decode.
	img := image.NewRGBA(image.Rect(0, 0, 120, 90))
	for y := range 90 {
		for x := range 120 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "photo.jpg"), buf.Bytes())
	write(t, filepath.Join(root, "notes.txt"), []byte("plain text"))
	write(t, filepath.Join(root, "page.html"), []byte("<script>alert(1)</script>"))
	if err := os.Mkdir(filepath.Join(root, "trip"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "trip", "clip.mp4"), []byte("fake-mp4-bytes"))

	cfg := &config.Config{
		Root: root, CacheDir: cacheDir, Title: "Test Drive",
		Users: map[string]string{"admin": testPassword}, SessionTTL: time.Hour,
		ThumbPx: 200, ThumbJobs: 2, EnableZip: true,
	}
	cache, err := thumbs.NewCache(cacheDir, cfg.ThumbPx, cfg.ThumbJobs)
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(cfg.Users, []byte("unit-test-key"), cfg.SessionTTL, false, false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, vault.New(root), a, cache, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), root
}

func write(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// do issues a request with Basic auth unless anonymous is requested.
func do(t *testing.T, h http.Handler, method, target string, authed bool) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Accept", "text/html")
	if authed {
		r.SetBasicAuth("admin", testPassword)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func pathURL(base, p string) string {
	return base + "?p=" + strings.ReplaceAll(url.QueryEscape(p), "+", "%20")
}

func TestHealthzIsPublic(t *testing.T) {
	h, _ := newTestServer(t)
	res := do(t, h, http.MethodGet, "/healthz", false)
	if res.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", res.StatusCode)
	}
}

func TestUnauthenticatedAccess(t *testing.T) {
	h, _ := newTestServer(t)

	// A browser navigation is redirected to the login page.
	res := do(t, h, http.MethodGet, "/browse", false)
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("browse without auth = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("redirect went to %q, want /login", loc)
	}

	// A non-browser client gets a plain 401 with a challenge.
	r := httptest.NewRequest(http.MethodGet, "/raw?p=photo.jpg", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("raw without auth = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge")
	}
}

func TestLoginFlow(t *testing.T) {
	h, _ := newTestServer(t)

	form := url.Values{"password": {testPassword}, "next": {"/browse"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", w.Code)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}

	// The cookie alone must now grant access.
	r2 := httptest.NewRequest(http.MethodGet, "/browse", nil)
	r2.AddCookie(session)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("browse with session = %d, want 200", w2.Code)
	}

	// A wrong password does not.
	bad := url.Values{"password": {"nope"}}
	r3 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(bad.Encode()))
	r3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, r3)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("bad login = %d, want 401", w3.Code)
	}
}

func TestBrowseListsEntries(t *testing.T) {
	h, _ := newTestServer(t)
	res := do(t, h, http.MethodGet, "/browse", true)
	body, _ := io.ReadAll(res.Body)
	page := string(body)

	for _, want := range []string{"photo.jpg", "notes.txt", "trip"} {
		if !strings.Contains(page, want) {
			t.Errorf("listing is missing %q", want)
		}
	}
	if !strings.Contains(page, "/thumb?p=photo.jpg") {
		t.Error("no thumbnail link for the JPEG")
	}
	// The UI must stay free of inline script for the CSP to hold.
	if strings.Contains(page, "<script>") {
		t.Error("page contains an inline <script> block")
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	h, _ := newTestServer(t)

	for _, probe := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"../../../../../../etc/passwd",
		"trip/../../etc/passwd",
	} {
		res := do(t, h, http.MethodGet, pathURL("/raw", probe), true)
		if res.StatusCode == http.StatusOK {
			t.Errorf("traversal %q returned 200", probe)
		}
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "root:") {
			t.Fatalf("traversal %q leaked /etc/passwd", probe)
		}
	}
}

func TestContentDisposition(t *testing.T) {
	h, _ := newTestServer(t)

	cases := []struct {
		url, wantType, wantDisp string
	}{
		{pathURL("/raw", "photo.jpg"), "image/jpeg", "inline"},
		{pathURL("/dl", "photo.jpg"), "image/jpeg", "attachment"},
		{pathURL("/raw", "trip/clip.mp4"), "video/mp4", "inline"},
		// Active content must never render in this origin.
		{pathURL("/raw", "page.html"), "text/html", "attachment"},
		{pathURL("/raw", "notes.txt"), "text/plain", "attachment"},
	}
	for _, c := range cases {
		res := do(t, h, http.MethodGet, c.url, true)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", c.url, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.wantType) {
			t.Errorf("%s Content-Type = %q, want %q", c.url, ct, c.wantType)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, c.wantDisp) {
			t.Errorf("%s Content-Disposition = %q, want %q", c.url, cd, c.wantDisp)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _ := newTestServer(t)
	res := do(t, h, http.MethodGet, "/browse", true)

	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q (got %q)", want, csp)
		}
	}
}

func TestThumbnail(t *testing.T) {
	h, _ := newTestServer(t)

	res := do(t, h, http.MethodGet, pathURL("/thumb", "photo.jpg"), true)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("thumb = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("thumb Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("thumbnail did not decode: %v", err)
	}

	// Formats without a decoder have no preview.
	if res := do(t, h, http.MethodGet, pathURL("/thumb", "notes.txt"), true); res.StatusCode != http.StatusNotFound {
		t.Errorf("thumb of a text file = %d, want 404", res.StatusCode)
	}
}

func TestRangeRequest(t *testing.T) {
	h, _ := newTestServer(t)

	r := httptest.NewRequest(http.MethodGet, pathURL("/raw", "trip/clip.mp4"), nil)
	r.SetBasicAuth("admin", testPassword)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusPartialContent {
		t.Errorf("range request = %d, want 206", w.Code)
	}
	if got := w.Body.Len(); got != 4 {
		t.Errorf("range returned %d bytes, want 4", got)
	}
}

func TestZipFolder(t *testing.T) {
	h, _ := newTestServer(t)

	res := do(t, h, http.MethodGet, pathURL("/zip", "trip"), true)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("zip = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip Content-Type = %q", ct)
	}

	body, _ := io.ReadAll(res.Body)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("archive is not readable: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "clip.mp4" {
		t.Errorf("unexpected archive contents: %+v", zr.File)
	}
}

func TestSearch(t *testing.T) {
	h, _ := newTestServer(t)

	res := do(t, h, http.MethodGet, "/search?q=clip", true)
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "clip.mp4") {
		t.Error("search did not find clip.mp4 in a subfolder")
	}

	res = do(t, h, http.MethodGet, "/search?q=nothingmatches", true)
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), "Nothing matched") {
		t.Error("empty search should show the empty state")
	}
}

func TestAPIList(t *testing.T) {
	h, _ := newTestServer(t)
	res := do(t, h, http.MethodGet, "/api/list", true)

	var payload struct {
		Path    string        `json:"path"`
		Entries []vault.Entry `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	if len(payload.Entries) != 4 {
		t.Errorf("got %d entries, want 4", len(payload.Entries))
	}
	if !payload.Entries[0].IsDir {
		t.Error("folders should sort first in the API too")
	}
}

func TestBrowsingAFileRedirectsToParent(t *testing.T) {
	h, _ := newTestServer(t)
	res := do(t, h, http.MethodGet, pathURL("/browse", "trip/clip.mp4"), true)
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("browse of a file = %d, want 303", res.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                     "/browse",
		"/browse?p=x":          "/browse?p=x",
		"//evil.example.com":   "/browse",
		"https://evil.example": "/browse",
		"javascript:alert(1)":  "/browse",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB",
		1536: "1.5 KB", 1048576: "1.0 MB", 1073741824: "1.0 GB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
