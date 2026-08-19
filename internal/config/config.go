// Package config loads runtime settings from the environment.
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the server needs.
type Config struct {
	Root       string // absolute path of the mounted folder, served read-only
	CacheDir   string // where generated thumbnails live (never inside Root)
	Addr       string
	Title      string
	Users      map[string]string // username -> password
	Anonymous  bool              // serve without a login (opt-in only)
	SecureCk   bool              // set Secure on the session cookie (behind TLS)
	Session    []byte            // HMAC key for session cookies
	SessionTTL time.Duration
	ThumbPx    int // longest edge of a generated thumbnail
	ThumbJobs  int // max concurrent thumbnail decodes
	EnableZip  bool
	MaxZipMB   int64 // 0 = unlimited
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseUsers reads "alice:secret,bob:hunter2" into a map.
func parseUsers(raw string) (map[string]string, error) {
	users := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, pass, ok := strings.Cut(pair, ":")
		name, pass = strings.TrimSpace(name), strings.TrimSpace(pass)
		if !ok || name == "" || pass == "" {
			return nil, fmt.Errorf("DRIVELITE_USERS entry %q is not in user:password form", pair)
		}
		users[name] = pass
	}
	return users, nil
}

// Load builds a Config from the environment, failing fast on anything unsafe.
func Load() (*Config, error) {
	c := &Config{
		Addr:       env("DRIVELITE_ADDR", ":8080"),
		Title:      env("DRIVELITE_TITLE", "Drive"),
		Anonymous:  envBool("DRIVELITE_ALLOW_ANONYMOUS", false),
		SecureCk:   envBool("DRIVELITE_SECURE_COOKIE", false),
		SessionTTL: time.Duration(envInt("DRIVELITE_SESSION_HOURS", 168)) * time.Hour,
		ThumbPx:    envInt("DRIVELITE_THUMB_PX", 400),
		ThumbJobs:  envInt("DRIVELITE_THUMB_JOBS", 4),
		EnableZip:  envBool("DRIVELITE_ENABLE_ZIP", true),
		MaxZipMB:   int64(envInt("DRIVELITE_MAX_ZIP_MB", 0)),
	}

	root, err := filepath.Abs(env("DRIVELITE_ROOT", "/data"))
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	// Follow symlinks now so every later containment check compares real paths.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("root %q is not readable: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", root)
	}
	c.Root = root

	c.CacheDir = env("DRIVELITE_CACHE_DIR", "/cache")
	if abs, err := filepath.Abs(c.CacheDir); err == nil {
		c.CacheDir = abs
	}
	// A cache inside the served tree would leak thumbnails into listings and
	// make the "we never write to Root" guarantee untrue.
	if c.CacheDir == c.Root || strings.HasPrefix(c.CacheDir, c.Root+string(os.PathSeparator)) {
		return nil, fmt.Errorf("cache dir %q must live outside the served root %q", c.CacheDir, c.Root)
	}

	if raw := os.Getenv("DRIVELITE_USERS"); strings.TrimSpace(raw) != "" {
		if c.Users, err = parseUsers(raw); err != nil {
			return nil, err
		}
	} else if pw := strings.TrimSpace(os.Getenv("DRIVELITE_PASSWORD")); pw != "" {
		c.Users = map[string]string{env("DRIVELITE_USER", "admin"): pw}
	} else {
		c.Users = map[string]string{}
	}

	if len(c.Users) == 0 && !c.Anonymous {
		return nil, fmt.Errorf("no credentials configured: set DRIVELITE_PASSWORD (or DRIVELITE_USERS), " +
			"or set DRIVELITE_ALLOW_ANONYMOUS=true to deliberately serve without a login")
	}

	if key := strings.TrimSpace(os.Getenv("DRIVELITE_SESSION_KEY")); key != "" {
		c.Session = []byte(key)
	} else {
		// Ephemeral key: sessions simply do not survive a restart.
		c.Session = make([]byte, 32)
		if _, err := rand.Read(c.Session); err != nil {
			return nil, fmt.Errorf("generating session key: %w", err)
		}
	}

	if c.ThumbPx < 32 {
		c.ThumbPx = 32
	}
	if c.ThumbJobs < 1 {
		c.ThumbJobs = 1
	}
	return c, nil
}
