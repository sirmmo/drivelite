// Package httpd wires the HTTP surface: routing, auth middleware and rendering.
package httpd

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"drivelite/internal/auth"
	"drivelite/internal/config"
	"drivelite/internal/source"
	"drivelite/internal/thumbs"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server holds everything the handlers need.
type Server struct {
	cfg   *config.Config
	src   source.Source
	auth  *auth.Authenticator
	thumb *thumbs.Cache
	tmpl  *template.Template
	log   *slog.Logger
}

// New builds a Server and parses the embedded templates.
func New(cfg *config.Config, src source.Source, a *auth.Authenticator, t *thumbs.Cache, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanSize": humanSize,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	return &Server{cfg: cfg, src: src, auth: a, thumb: t, tmpl: tmpl, log: log}, nil
}

// Handler returns the fully wired root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err) // embedded FS layout is fixed at compile time
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheForever(http.FileServer(http.FS(static)))))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Everything below requires an authenticated caller.
	mux.Handle("GET /{$}", s.protect(http.HandlerFunc(s.handleBrowse)))
	mux.Handle("GET /browse", s.protect(http.HandlerFunc(s.handleBrowse)))
	mux.Handle("GET /search", s.protect(http.HandlerFunc(s.handleSearch)))
	mux.Handle("GET /raw", s.protect(http.HandlerFunc(s.handleRaw)))
	mux.Handle("GET /dl", s.protect(http.HandlerFunc(s.handleDownload)))
	mux.Handle("GET /thumb", s.protect(http.HandlerFunc(s.handleThumb)))
	mux.Handle("GET /zip", s.protect(http.HandlerFunc(s.handleZip)))
	mux.Handle("GET /api/list", s.protect(http.HandlerFunc(s.handleAPIList)))

	return s.baseHeaders(s.logRequests(mux))
}

// protect rejects unauthenticated callers, redirecting browsers to the login
// page and answering API/asset requests with a plain 401.
func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.Identify(r); !ok {
			if wantsHTML(r) {
				dest := r.URL.RequestURI()
				http.Redirect(w, r, "/login?next="+url.QueryEscape(dest), http.StatusSeeOther)
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="drivelite", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// baseHeaders applies defensive headers to every response.
func (s *Server) baseHeaders(next http.Handler) http.Handler {
	// Everything is same-origin; no inline script is used anywhere.
	const csp = "default-src 'self'; img-src 'self' data:; media-src 'self'; " +
		"object-src 'none'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so streaming responses (ZIP) work.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r))
	})
}

func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

// wantsHTML reports whether the caller looks like a browser navigating.
func wantsHTML(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") == "fetch" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// humanSize renders a byte count in familiar units.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
