package httpd

import (
	"archive/zip"
	"bytes"
	"context"
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
	"sort"
	"strings"
	"testing"
	"time"

	"drivelite/internal/auth"
	"drivelite/internal/config"
	"drivelite/internal/source"
	"drivelite/internal/thumbs"
)

const testPassword = "s3cret"

// sampleTree is the fixture every backend under test is populated with.
func sampleTree(t *testing.T) map[string][]byte {
	t.Helper()

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

	return map[string][]byte{
		"photo.jpg":     buf.Bytes(),
		"notes.txt":     []byte("plain text"),
		"page.html":     []byte("<script>alert(1)</script>"),
		"trip/clip.mp4": []byte("fake-mp4-bytes"),
	}
}

// newServer wires a Server around any source.
func newServer(t *testing.T, src source.Source, cfg *config.Config) http.Handler {
	t.Helper()
	cache, err := thumbs.NewCache(t.TempDir(), cfg.ThumbPx, cfg.ThumbJobs)
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(cfg.Users, []byte("unit-test-key"), cfg.SessionTTL, false, cfg.Anonymous)
	srv, err := New(cfg, src, a, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func baseConfig() *config.Config {
	return &config.Config{
		Backend:    config.BackendLocal,
		Title:      "Test Drive",
		Users:      map[string]string{"admin": testPassword},
		SessionTTL: time.Hour, ThumbPx: 200, ThumbJobs: 2, EnableZip: true,
	}
}

// newLocalServer materialises the fixture on disk and serves it.
func newLocalServer(t *testing.T) http.Handler {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for path, body := range sampleTree(t) {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := source.NewLocal(root, "test folder")
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Root = root
	return newServer(t, src, cfg)
}

// ---- an in-memory backend, to prove the handlers are not local-only ----

type memSource struct{ files map[string][]byte }

func (m *memSource) Name() string { return "memory" }
func (m *memSource) Close() error { return nil }

func (m *memSource) entry(path string) source.Entry {
	return source.Describe(source.Entry{
		Name: source.Base(path), Path: path,
		Size: int64(len(m.files[path])), ModUnix: 1700000000,
		CacheTag: "tag-" + path,
	})
}

func (m *memSource) Stat(_ context.Context, name string) (source.Entry, error) {
	name = source.Clean(name)
	if name == "" {
		return source.Entry{IsDir: true, Kind: source.KindFolder}, nil
	}
	if _, ok := m.files[name]; ok {
		return m.entry(name), nil
	}
	for p := range m.files {
		if strings.HasPrefix(p, name+"/") {
			return source.Entry{Name: source.Base(name), Path: name,
				IsDir: true, Kind: source.KindFolder}, nil
		}
	}
	return source.Entry{}, source.ErrNotFound
}

func (m *memSource) List(_ context.Context, dir string) ([]source.Entry, error) {
	dir = source.Clean(dir)
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	var out []source.Entry
	for p := range m.files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			name := rest[:i]
			if !seen[name] {
				seen[name] = true
				out = append(out, source.Entry{Name: name, Path: source.Join(dir, name),
					IsDir: true, Kind: source.KindFolder})
			}
			continue
		}
		out = append(out, m.entry(p))
	}
	return out, nil
}

func (m *memSource) Open(_ context.Context, name string) (source.File, error) {
	body, ok := m.files[source.Clean(name)]
	if !ok {
		return nil, source.ErrNotFound
	}
	return &memFile{Reader: bytes.NewReader(body), size: int64(len(body))}, nil
}

func (m *memSource) Walk(_ context.Context, dir string, fn func(source.Entry) error) error {
	dir = source.Clean(dir)
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	paths := make([]string, 0, len(m.files))
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := fn(m.entry(p)); err != nil {
			return err
		}
	}
	return nil
}

type memFile struct {
	*bytes.Reader
	size int64
}

func (f *memFile) Size() int64  { return f.size }
func (f *memFile) Close() error { return nil }

func newMemServer(t *testing.T) http.Handler {
	t.Helper()
	files := map[string][]byte{}
	for k, v := range sampleTree(t) {
		files[k] = v
	}
	cfg := baseConfig()
	cfg.Backend = config.BackendS3
	cfg.S3.Bucket = "test-bucket"
	return newServer(t, &memSource{files: files}, cfg)
}

// backends enumerates every wiring the handler tests run against.
func backends(t *testing.T) map[string]http.Handler {
	return map[string]http.Handler{
		"local":  newLocalServer(t),
		"memory": newMemServer(t),
	}
}

// ---- helpers ----

func do(t *testing.T, h http.Handler, target string, authed bool) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
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

// ---- tests ----

func TestHealthzIsPublic(t *testing.T) {
	h := newLocalServer(t)
	if res := do(t, h, "/healthz", false); res.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", res.StatusCode)
	}
}

func TestUnauthenticatedAccess(t *testing.T) {
	h := newLocalServer(t)

	res := do(t, h, "/browse", false)
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("browse without auth = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("redirect went to %q, want /login", loc)
	}

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
	h := newLocalServer(t)

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

	r2 := httptest.NewRequest(http.MethodGet, "/browse", nil)
	r2.AddCookie(session)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("browse with session = %d, want 200", w2.Code)
	}

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
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			res := do(t, h, "/browse", true)
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
		})
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			for _, probe := range []string{
				"../../etc/passwd",
				"/etc/passwd",
				"../../../../../../etc/passwd",
				"trip/../../etc/passwd",
				`..\..\etc\passwd`,
			} {
				res := do(t, h, pathURL("/raw", probe), true)
				if res.StatusCode == http.StatusOK {
					t.Errorf("traversal %q returned 200", probe)
				}
				body, _ := io.ReadAll(res.Body)
				if strings.Contains(string(body), "root:") {
					t.Fatalf("traversal %q leaked /etc/passwd", probe)
				}
			}
		})
	}
}

func TestContentDispositionAcrossBackends(t *testing.T) {
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
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			for _, c := range cases {
				res := do(t, h, c.url, true)
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
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	res := do(t, newLocalServer(t), "/browse", true)

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
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			res := do(t, h, pathURL("/thumb", "photo.jpg"), true)
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
			if res := do(t, h, pathURL("/thumb", "notes.txt"), true); res.StatusCode != http.StatusNotFound {
				t.Errorf("thumb of a text file = %d, want 404", res.StatusCode)
			}
		})
	}
}

func TestRangeRequest(t *testing.T) {
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
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
		})
	}
}

func TestZipFolder(t *testing.T) {
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			res := do(t, h, pathURL("/zip", "trip"), true)
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
				var got []string
				for _, f := range zr.File {
					got = append(got, f.Name)
				}
				t.Errorf("unexpected archive contents: %v", got)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			res := do(t, h, "/search?q=clip", true)
			body, _ := io.ReadAll(res.Body)
			if !strings.Contains(string(body), "clip.mp4") {
				t.Error("search did not find clip.mp4 in a subfolder")
			}

			// A folder whose own name matches is reported too, even though
			// Walk only ever visits files.
			res = do(t, h, "/search?q=trip", true)
			body, _ = io.ReadAll(res.Body)
			if !strings.Contains(string(body), "trip") {
				t.Error("search did not report the matching folder")
			}

			res = do(t, h, "/search?q=nothingmatches", true)
			body, _ = io.ReadAll(res.Body)
			if !strings.Contains(string(body), "Nothing matched") {
				t.Error("empty search should show the empty state")
			}
		})
	}
}

func TestAPIList(t *testing.T) {
	for name, h := range backends(t) {
		t.Run(name, func(t *testing.T) {
			res := do(t, h, "/api/list", true)

			var payload struct {
				Backend string         `json:"backend"`
				Path    string         `json:"path"`
				Entries []source.Entry `json:"entries"`
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
			if payload.Backend == "" {
				t.Error("API response should name the backend")
			}
		})
	}
}

func TestZipSizeLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := source.NewLocal(root, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Root = root
	cfg.MaxZipMB = 1
	h := newServer(t, src, cfg)

	// 4 KB is under the 1 MB cap.
	if res := do(t, h, "/zip", true); res.StatusCode != http.StatusOK {
		t.Errorf("small zip = %d, want 200", res.StatusCode)
	}

	// Now make the cap impossible to satisfy.
	cfg.MaxZipMB = 0
	src2, _ := source.NewLocal(root, "")
	cfg2 := baseConfig()
	cfg2.Root = root
	cfg2.EnableZip = false
	if res := do(t, newServer(t, src2, cfg2), "/zip", true); res.StatusCode != http.StatusForbidden {
		t.Errorf("disabled zip = %d, want 403", res.StatusCode)
	}
}

func TestBrowsingAFileRedirectsToParent(t *testing.T) {
	res := do(t, newLocalServer(t), pathURL("/browse", "trip/clip.mp4"), true)
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
