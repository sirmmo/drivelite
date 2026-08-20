package httpd

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"drivelite/internal/source"
)

// viewEntry decorates an entry with everything the template needs.
type viewEntry struct {
	source.Entry
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
	Backend   string
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

func (s *Server) decorate(e source.Entry) viewEntry {
	v := viewEntry{Entry: e}
	if e.ModUnix > 0 {
		v.ModText = time.Unix(e.ModUnix, 0).Format("2 Jan 2006")
	}
	if e.IsDir {
		v.URL = assetURL("/browse", e.Path)
		v.Icon = "folder"
		return v
	}
	v.RawURL = assetURL("/raw", e.Path)
	v.DlURL = assetURL("/dl", e.Path)
	v.URL = v.DlURL
	v.SizeText = humanSize(e.Size)
	if e.Previewable {
		v.ThumbURL = assetURL("/thumb", e.Path)
	}
	switch e.Kind {
	case source.KindImage:
		v.Icon = "image"
	case source.KindVideo:
		v.Icon = "video"
	case source.KindAudio:
		v.Icon = "audio"
	case source.KindPDF:
		v.Icon = "pdf"
	case source.KindText:
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
		acc = source.Join(acc, part)
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
	ctx := r.Context()
	rel := source.Clean(r.URL.Query().Get("p"))

	entry, err := s.src.Stat(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Asking to browse a file just shows its parent instead of erroring.
	if !entry.IsDir {
		http.Redirect(w, r, assetURL("/browse", source.Parent(rel)), http.StatusSeeOther)
		return
	}

	sortMode, dir, view := prefs(r)
	entries, err := s.src.List(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	source.Sort(entries, source.SortMode(sortMode), dir == "desc")
	persistPrefs(w, sortMode, dir, view)

	page := s.buildPage(r, rel, entries, sortMode, dir, view)
	page.Heading = "Home"
	if rel != "" {
		page.Heading = source.Base(rel)
	}
	s.render(w, r, "browse.html", page)
}

func (s *Server) buildPage(r *http.Request, rel string, entries []source.Entry, sortMode, dir, view string) *browsePage {
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
		Backend:   s.src.Name(),
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
		p.ParentURL = assetURL("/browse", source.Parent(rel))
	}
	return p
}

// handleSearch walks the tree below a folder looking for name matches.
//
// Walk reports files only — that is the model S3 imposes, since its
// "directories" are just shared key prefixes. Folder matches are therefore
// derived from the ancestors of matching paths, which keeps behaviour
// identical across all three backends.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	const maxResults = 400

	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	rel := source.Clean(r.URL.Query().Get("p"))
	if query == "" {
		http.Redirect(w, r, assetURL("/browse", rel), http.StatusSeeOther)
		return
	}

	if _, err := s.src.Stat(ctx, rel); err != nil {
		s.fail(w, r, err)
		return
	}

	needle := strings.ToLower(query)
	var found []source.Entry
	seenDirs := map[string]bool{}
	truncated := false

	errStop := errors.New("enough results")
	err := s.src.Walk(ctx, rel, func(e source.Entry) error {
		// Any ancestor folder whose own name matches counts as a hit.
		for dir := source.Parent(e.Path); dir != "" && dir != rel; dir = source.Parent(dir) {
			if seenDirs[dir] {
				break
			}
			seenDirs[dir] = true
			if strings.Contains(strings.ToLower(source.Base(dir)), needle) {
				found = append(found, source.Entry{
					Name: source.Base(dir), Path: dir, IsDir: true, Kind: source.KindFolder,
				})
			}
		}

		if strings.Contains(strings.ToLower(e.Name), needle) {
			found = append(found, e)
		}
		if len(found) >= maxResults {
			truncated = true
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		s.fail(w, r, err)
		return
	}

	sortMode, dir, view := prefs(r)
	source.Sort(found, source.SortMode(sortMode), dir == "desc")

	page := s.buildPage(r, rel, found, sortMode, dir, view)
	page.IsSearch = true
	page.Query = query
	page.Truncated = truncated
	page.Heading = fmt.Sprintf("%d result(s) for %q", len(found), query)
	page.HasParent = true
	page.ParentURL = assetURL("/browse", rel)
	s.render(w, r, "browse.html", page)
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rel := source.Clean(r.URL.Query().Get("p"))
	sortMode, dir, _ := prefs(r)

	entries, err := s.src.List(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	source.Sort(entries, source.SortMode(sortMode), dir == "desc")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"backend": s.src.Name(),
		"path":    rel,
		"entries": entries,
	})
}

// serveFile streams a file with Range support.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, attachment bool) {
	ctx := r.Context()
	rel := source.Clean(r.URL.Query().Get("p"))

	entry, err := s.src.Stat(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entry.IsDir {
		s.fail(w, r, errors.New("cannot download a directory directly"))
		return
	}

	f, err := s.src.Open(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()

	// Only media is ever served inline. Anything else — HTML, SVG, scripts —
	// is forced to download so it cannot execute in this origin.
	inline := !attachment && entry.Kind != source.KindOther &&
		entry.Kind != source.KindText && entry.MimeType != "image/svg+xml"
	disp := "attachment"
	if inline {
		disp = "inline"
	}

	w.Header().Set("Content-Type", entry.MimeType)
	w.Header().Set("Content-Disposition", contentDisposition(disp, entry.Name))
	w.Header().Set("Cache-Control", "private, max-age=300")
	if entry.CacheTag != "" {
		w.Header().Set("ETag", `"`+entry.CacheTag+`"`)
	}
	http.ServeContent(w, r, entry.Name, time.Unix(entry.ModUnix, 0), f)
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, false)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, true)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rel := source.Clean(r.URL.Query().Get("p"))

	entry, err := s.src.Stat(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entry.IsDir || !entry.Previewable {
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}

	edge := s.cfg.ThumbPx
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			edge = n
		}
	}

	// The identity must change when the bytes change, but stay stable across
	// restarts and (for git) across commits that did not touch this file.
	identity := s.scope() + "|" + entry.Path + "|" + entry.CacheTag

	thumbPath, err := s.thumb.Get(identity, edge, func() (io.ReadCloser, error) {
		return s.src.Open(ctx, rel)
	})
	if err != nil {
		s.log.Warn("thumbnail failed", "path", rel, "err", err)
		http.Error(w, "no preview available", http.StatusNotFound)
		return
	}

	// The generated thumbnail always lives on the local cache disk, whatever
	// the backing store is.
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

// scope identifies the backing store, so thumbnails cached for one source are
// never reused for another.
func (s *Server) scope() string {
	switch s.cfg.Backend {
	case "s3":
		return "s3|" + s.cfg.S3.Bucket + "|" + s.cfg.S3.Prefix
	case "git":
		return "git|" + s.cfg.Git.URL + "|" + s.cfg.Git.Subdir
	default:
		return "local|" + s.cfg.Root
	}
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

	ctx := r.Context()
	rel := source.Clean(r.URL.Query().Get("p"))

	entry, err := s.src.Stat(ctx, rel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !entry.IsDir {
		http.Redirect(w, r, assetURL("/dl", rel), http.StatusSeeOther)
		return
	}

	if s.cfg.MaxZipMB > 0 {
		limit := s.cfg.MaxZipMB << 20
		over, err := s.exceedsLimit(ctx, rel, limit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if over {
			http.Error(w, fmt.Sprintf("folder exceeds the %d MB archive limit", s.cfg.MaxZipMB),
				http.StatusRequestEntityTooLarge)
			return
		}
	}

	name := source.Base(rel)
	if rel == "" {
		name = "drive"
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name+".zip"))
	// Length is unknown up front; the archive streams out as it is built.

	zw := zip.NewWriter(w)
	defer zw.Close()

	walkErr := s.src.Walk(ctx, rel, func(e source.Entry) error {
		inner := strings.TrimPrefix(e.Path, rel)
		inner = strings.TrimPrefix(inner, "/")
		if inner == "" {
			return nil
		}

		hdr := &zip.FileHeader{
			Name:     inner,
			Method:   zip.Deflate,
			Modified: time.Unix(e.ModUnix, 0),
		}
		if storeExt[strings.ToLower(extOf(e.Name))] {
			hdr.Method = zip.Store
		}

		dst, err := zw.CreateHeader(hdr)
		if err != nil {
			return err // client disconnected
		}
		src, err := s.src.Open(ctx, e.Path)
		if err != nil {
			// A file that vanished mid-archive should not abort the download.
			s.log.Warn("skipping file in archive", "path", e.Path, "err", err)
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

// exceedsLimit adds up a subtree, stopping as soon as the limit is passed.
func (s *Server) exceedsLimit(ctx context.Context, dir string, limit int64) (bool, error) {
	var total int64
	over := false
	stop := errors.New("over limit")

	err := s.src.Walk(ctx, dir, func(e source.Entry) error {
		total += e.Size
		if total > limit {
			over = true
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return false, err
	}
	return over, nil
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
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
	case errors.Is(err, source.ErrForbidden):
		s.log.Warn("blocked path", "query", r.URL.RawQuery, "remote", clientIP(r))
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, source.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, context.Canceled):
		// The client went away; nothing to report.
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
