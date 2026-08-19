package httpd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"drivelite/internal/vault"
)

// viewEntry decorates a vault entry with everything the template needs.
type viewEntry struct {
	vault.Entry
	URL      string // where a click leads
	RawURL   string
	DlURL    string
	ThumbURL string
	SizeText string
	ModText  string
	Icon     string
}

type crumb struct {
	Name string
	URL  string
	Last bool
}

type browsePage struct {
	Title     string
	Heading   string
	User      string
	Anonymous bool
	Path      string
	Crumbs    []crumb
	Entries   []viewEntry
	ParentURL string
	HasParent bool
	ZipURL    string
	EnableZip bool
	Sort      string
	Dir       string
	View      string
	Query     string
	IsSearch  bool
	Folders   int
	Files     int
	TotalSize string
	Truncated bool
}

// assetURL builds a link carrying the path in the query string, which keeps
// characters like '#' and '%' in filenames from breaking URL parsing.
//
// Spaces are emitted as %20 rather than '+'. Both decode identically server
// side, but '+' becomes the entity &#43; once html/template escapes it into an
// attribute, which makes the rendered URLs awkward to copy or script against.
// QueryEscape turns a literal '+' in a filename into %2B first, so every '+'
// left in the output came from a space and is safe to rewrite.
func assetURL(base, p string) string {
	if p == "" {
		return base
	}
	return base + "?p=" + strings.ReplaceAll(url.QueryEscape(p), "+", "%20")
}

func (s *Server) decorate(e vault.Entry) viewEntry {
	v := viewEntry{Entry: e}
	if e.IsDir {
		v.URL = assetURL("/browse", e.Path)
		v.Icon = "folder"
		v.ModText = time.Unix(e.ModUnix, 0).Format("2 Jan 2006")
		return v
	}
	v.RawURL = assetURL("/raw", e.Path)
	v.DlURL = assetURL("/dl", e.Path)
	v.URL = v.DlURL
	v.SizeText = humanSize(e.Size)
	v.ModText = time.Unix(e.ModUnix, 0).Format("2 Jan 2006")
	if e.Previewable {
		v.ThumbURL = assetURL("/thumb", e.Path)
	}
	switch e.Kind {
	case vault.KindImage:
		v.Icon = "image"
	case vault.KindVideo:
		v.Icon = "video"
	case vault.KindAudio:
		v.Icon = "audio"
	case vault.KindPDF:
		v.Icon = "pdf"
	case vault.KindText:
		v.Icon = "text"
	default:
		v.Icon = "file"
	}
	return v
}

func crumbsFor(p string) []crumb {
	out := []crumb{{Name: "Home", URL: "/browse"}}
	if p == "" {
		out[0].Last = true
		return out
	}
	parts := strings.Split(p, "/")
	acc := ""
	for i, part := range parts {
		if acc == "" {
			acc = part
		} else {
			acc = acc + "/" + part
		}
		out = append(out, crumb{Name: part, URL: assetURL("/browse", acc), Last: i == len(parts)-1})
	}
	return out
}

// prefs reads the sort/view preferences from query or cookie.
func prefs(r *http.Request) (sortMode, dir, view string) {
	pick := func(name, def string, valid ...string) string {
		v := r.URL.Query().Get(name)
		if v == "" {
			if c, err := r.Cookie("dl_" + name); err == nil {
				v = c.Value
			}
		}
		for _, ok := range valid {
			if v == ok {
				return v
			}
		}
		return def
	}
	return pick("sort", "name", "name", "size", "date"),
		pick("dir", "asc", "asc", "desc"),
		pick("view", "grid", "grid", "list")
}

func persistPrefs(w http.ResponseWriter, sortMode, dir, view string) {
	for name, val := range map[string]string{"sort": sortMode, "dir": dir, "view": view} {
		http.SetCookie(w, &http.Cookie{
			Name: "dl_" + name, Value: val, Path: "/",
			MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	rel := vault.Clean(r.URL.Query().Get("p"))

	abs, info, err := s.vault.Stat(rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Asking to browse a file just shows its parent instead of erroring.
	if !info.IsDir() {
		http.Redirect(w, r, assetURL("/browse", path.Dir(rel)), http.StatusSeeOther)
		return
	}
	_ = abs

	sortMode, dir, view := prefs(r)
	entries, err := s.vault.List(rel, vault.SortMode(sortMode), dir == "desc")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	persistPrefs(w, sortMode, dir, view)

	page := s.buildPage(r, rel, entries, sortMode, dir, view)
	page.Heading = "Home"
	if rel != "" {
		page.Heading = path.Base(rel)
	}
	s.render(w, r, "browse.html", page)
}

func (s *Server) buildPage(r *http.Request, rel string, entries []vault.Entry, sortMode, dir, view string) *browsePage {
	user, _ := s.auth.Identify(r)

	views := make([]viewEntry, 0, len(entries))
	var folders, files int
	var total int64
	for _, e := range entries {
		views = append(views, s.decorate(e))
		if e.IsDir {
			folders++
		} else {
			files++
			total += e.Size
		}
	}

	p := &browsePage{
		Title:     s.cfg.Title,
		User:      user,
		Anonymous: s.auth.Anonymous(),
		Path:      rel,
		Crumbs:    crumbsFor(rel),
		Entries:   views,
		EnableZip: s.cfg.EnableZip,
		ZipURL:    assetURL("/zip", rel),
		Sort:      sortMode,
		Dir:       dir,
		View:      view,
		Folders:   folders,
		Files:     files,
		TotalSize: humanSize(total),
	}
	if rel != "" {
		p.HasParent = true
		parent := path.Dir(rel)
		if parent == "." {
			parent = ""
		}
		p.ParentURL = assetURL("/browse", parent)
	}
	return p
}

// handleSearch walks the tree below a folder looking for name matches.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	const maxResults = 400

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	rel := vault.Clean(r.URL.Query().Get("p"))
	if query == "" {
		http.Redirect(w, r, assetURL("/browse", rel), http.StatusSeeOther)
		return
	}

	abs, _, err := s.vault.Stat(rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	needle := strings.ToLower(query)
	var found []vault.Entry
	truncated := false

	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree: skip rather than abort the search
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() && p != abs {
				return fs.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
		}
		if p == abs || !strings.Contains(strings.ToLower(d.Name()), needle) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return nil
		}
		childRel, err := s.vault.RelOf(p)
		if err != nil {
			return nil
		}
		found = append(found, describeFor(d.Name(), childRel, fi))
		if len(found) >= maxResults {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	sortMode, dir, view := prefs(r)
	page := s.buildPage(r, rel, found, sortMode, dir, view)
	page.IsSearch = true
	page.Query = query
	page.Truncated = truncated
	page.Heading = fmt.Sprintf("%d result(s) for %q", len(found), query)
	page.HasParent = true
	page.ParentURL = assetURL("/browse", rel)
	s.render(w, r, "browse.html", page)
}

// describeFor mirrors vault's entry construction for search hits.
func describeFor(name, rel string, fi os.FileInfo) vault.Entry {
	e := vault.Entry{
		Name: name, Path: rel, IsDir: fi.IsDir(), ModUnix: fi.ModTime().Unix(),
	}
	if fi.IsDir() {
		e.Kind = vault.KindFolder
		return e
	}
	e.Size = fi.Size()
	e.MimeType = vault.MimeOf(name)
	e.Previewable = vault.Decodable(name)
	switch {
	case strings.HasPrefix(e.MimeType, "image/"):
		e.Kind = vault.KindImage
	case strings.HasPrefix(e.MimeType, "video/"):
		e.Kind = vault.KindVideo
	case strings.HasPrefix(e.MimeType, "audio/"):
		e.Kind = vault.KindAudio
	case e.MimeType == "application/pdf":
		e.Kind = vault.KindPDF
	case strings.HasPrefix(e.MimeType, "text/"):
		e.Kind = vault.KindText
	default:
		e.Kind = vault.KindOther
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v", ".webm", ".ogv", ".mp3", ".m4a", ".ogg", ".wav", ".flac", ".opus":
		e.Playable = true
	}
	return e
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	rel := vault.Clean(r.URL.Query().Get("p"))
	sortMode, dir, _ := prefs(r)
	entries, err := s.vault.List(rel, vault.SortMode(sortMode), dir == "desc")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"path": rel, "entries": entries})
}

// serveFile streams a regular file with Range support.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, attachment bool) {
	rel := vault.Clean(r.URL.Query().Get("p"))
	abs, info, err := s.vault.Stat(rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if info.IsDir() {
		s.fail(w, r, errors.New("cannot download a directory directly"))
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		s.fail(w, r, vault.ErrNotFound)
		return
	}
	defer f.Close()

	name := filepath.Base(abs)
	mimeType := vault.MimeOf(name)
	kind := "other"
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		kind = "image"
	case strings.HasPrefix(mimeType, "video/"):
		kind = "video"
	case strings.HasPrefix(mimeType, "audio/"):
		kind = "audio"
	case mimeType == "application/pdf":
		kind = "pdf"
	}

	// Only media is ever served inline. Anything else — HTML, SVG, scripts —
	// is forced to download so it cannot execute in this origin.
	inline := !attachment && kind != "other" && mimeType != "image/svg+xml"
	disp := "attachment"
	if inline {
		disp = "inline"
	}

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", contentDisposition(disp, name))
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, false)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, true)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	rel := vault.Clean(r.URL.Query().Get("p"))
	abs, info, err := s.vault.Stat(rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if info.IsDir() || !vault.Decodable(filepath.Base(abs)) {
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}

	edge := s.cfg.ThumbPx
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			edge = n
		}
	}

	thumbPath, err := s.thumb.Get(abs, info.ModTime().Unix(), info.Size(), edge)
	if err != nil {
		s.log.Warn("thumbnail failed", "path", rel, "err", err)
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}

	f, err := os.Open(thumbPath)
	if err != nil {
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "thumb.jpg", fi.ModTime(), f)
}

// storeExt lists formats that are already compressed; re-deflating them in a
// ZIP costs CPU and saves nothing.
var storeExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".heic": true, ".avif": true, ".mp4": true, ".m4v": true, ".mov": true,
	".mts": true, ".m2ts": true, ".ts": true, ".mkv": true, ".webm": true,
	".mp3": true, ".m4a": true, ".flac": true, ".opus": true, ".zip": true,
	".gz": true, ".xz": true, ".7z": true, ".rar": true, ".pdf": true,
}

func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableZip {
		http.Error(w, "folder download is disabled", http.StatusForbidden)
		return
	}

	rel := vault.Clean(r.URL.Query().Get("p"))
	abs, info, err := s.vault.Stat(rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !info.IsDir() {
		http.Redirect(w, r, assetURL("/dl", rel), http.StatusSeeOther)
		return
	}

	if s.cfg.MaxZipMB > 0 {
		limit := s.cfg.MaxZipMB << 20
		if _, _, over := s.vault.DirSize(abs, limit); over {
			http.Error(w, fmt.Sprintf("folder exceeds the %d MB archive limit", s.cfg.MaxZipMB),
				http.StatusRequestEntityTooLarge)
			return
		}
	}

	name := path.Base(rel)
	if rel == "" {
		name = "drive"
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name+".zip"))
	// Length is unknown up front; the archive streams out as it is built.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	zw := zip.NewWriter(w)
	defer zw.Close()

	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}

		inner, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}

		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(inner)
		hdr.Method = zip.Deflate
		if storeExt[strings.ToLower(filepath.Ext(p))] {
			hdr.Method = zip.Store
		}

		dst, err := zw.CreateHeader(hdr)
		if err != nil {
			return err // client disconnected
		}
		src, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer src.Close()
		_, err = io.Copy(dst, src)
		return err
	})
	if walkErr != nil {
		s.log.Warn("zip stream ended early", "path", rel, "err", walkErr)
	}
}

// ---- authentication ----

type loginPage struct {
	Title    string
	Error    string
	Next     string
	SoleUser string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth.Identify(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	sole, _ := s.auth.SoleUser()
	s.render(w, r, "login.html", loginPage{
		Title:    s.cfg.Title,
		Next:     safeNext(r.URL.Query().Get("next")),
		SoleUser: sole,
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.FormValue("next"))

	if !s.auth.Allow(clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login.html", loginPage{
			Title: s.cfg.Title, Next: next,
			Error: "Too many attempts. Wait a moment and try again.",
		})
		return
	}

	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	// With a single configured account the username field may be left blank.
	if sole, ok := s.auth.SoleUser(); ok && user == "" {
		user = sole
	}

	if !s.auth.Check(user, pass) {
		s.log.Warn("failed login", "user", user, "remote", clientIP(r))
		w.WriteHeader(http.StatusUnauthorized)
		sole, _ := s.auth.SoleUser()
		s.render(w, r, "login.html", loginPage{
			Title: s.cfg.Title, Next: next, SoleUser: sole,
			Error: "Incorrect username or password.",
		})
		return
	}

	http.SetCookie(w, s.auth.Issue(user))
	s.log.Info("login", "user", user, "remote", clientIP(r))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.auth.Clear())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/browse"
	}
	return next
}

// ---- helpers ----

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render failed", "template", name, "err", err)
	}
	_ = r
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vault.ErrForbidden):
		s.log.Warn("blocked path", "query", r.URL.RawQuery, "remote", clientIP(r))
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, vault.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		s.log.Error("request failed", "err", err, "query", r.URL.RawQuery)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// contentDisposition builds a header safe for non-ASCII filenames.
func contentDisposition(disp, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s", disp, ascii, url.PathEscape(name))
}
