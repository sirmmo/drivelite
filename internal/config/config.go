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

// Backend names the kind of storage being exposed.
type Backend string

const (
	BackendLocal Backend = "local"
	BackendGit   Backend = "git"
	BackendS3    Backend = "s3"
)

// GitConfig describes a git-backed source.
type GitConfig struct {
	URL      string
	Ref      string
	Subdir   string
	Interval time.Duration
	Depth    int
	Username string
	Token    string
	WorkDir  string
}

// S3Config describes an S3-compatible source.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
	Token     string
	PathStyle bool
	CacheTTL  time.Duration
	Timeout   time.Duration
}

// Config holds every tunable the server needs.
type Config struct {
	Backend  Backend
	Root     string // local backend: the directory served, read-only
	Git      GitConfig
	S3       S3Config
	CacheDir string // where thumbnails and git checkouts live

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

// envDuration accepts either a Go duration ("5m") or a plain number of seconds.
func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
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

// detectBackend picks a backend from DRIVELITE_BACKEND, or infers one from
// whichever backend-specific settings are present.
func detectBackend() (Backend, error) {
	explicit := strings.ToLower(env("DRIVELITE_BACKEND", ""))
	switch explicit {
	case string(BackendLocal), string(BackendGit), string(BackendS3):
		return Backend(explicit), nil
	case "":
		// fall through to inference
	default:
		return "", fmt.Errorf("DRIVELITE_BACKEND %q is not one of local, git, s3", explicit)
	}

	hasS3 := env("DRIVELITE_S3_BUCKET", "") != ""
	hasGit := env("DRIVELITE_GIT_URL", "") != ""
	switch {
	case hasS3 && hasGit:
		return "", fmt.Errorf("both DRIVELITE_S3_BUCKET and DRIVELITE_GIT_URL are set: " +
			"choose one with DRIVELITE_BACKEND=s3 or DRIVELITE_BACKEND=git")
	case hasS3:
		return BackendS3, nil
	case hasGit:
		return BackendGit, nil
	}
	return BackendLocal, nil
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

	backend, err := detectBackend()
	if err != nil {
		return nil, err
	}
	c.Backend = backend

	c.CacheDir = env("DRIVELITE_CACHE_DIR", "/cache")
	if abs, err := filepath.Abs(c.CacheDir); err == nil {
		c.CacheDir = abs
	}

	switch backend {
	case BackendLocal:
		if err := c.loadLocal(); err != nil {
			return nil, err
		}
	case BackendGit:
		if err := c.loadGit(); err != nil {
			return nil, err
		}
	case BackendS3:
		if err := c.loadS3(); err != nil {
			return nil, err
		}
	}

	if err := c.loadAuth(); err != nil {
		return nil, err
	}

	if c.ThumbPx < 32 {
		c.ThumbPx = 32
	}
	if c.ThumbJobs < 1 {
		c.ThumbJobs = 1
	}
	return c, nil
}

func (c *Config) loadLocal() error {
	root, err := filepath.Abs(env("DRIVELITE_ROOT", "/data"))
	if err != nil {
		return fmt.Errorf("resolving root: %w", err)
	}
	// Follow symlinks now so every later containment check compares real paths.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("root %q is not readable: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %q is not a directory", root)
	}
	c.Root = root

	// A cache inside the served tree would leak thumbnails into listings and
	// make the "we never write to the served folder" guarantee untrue.
	if c.CacheDir == c.Root || strings.HasPrefix(c.CacheDir, c.Root+string(os.PathSeparator)) {
		return fmt.Errorf("cache dir %q must live outside the served root %q", c.CacheDir, c.Root)
	}
	return nil
}

func (c *Config) loadGit() error {
	c.Git = GitConfig{
		URL:      env("DRIVELITE_GIT_URL", ""),
		Ref:      env("DRIVELITE_GIT_REF", ""),
		Subdir:   env("DRIVELITE_GIT_SUBDIR", ""),
		Interval: envDuration("DRIVELITE_GIT_INTERVAL", 5*time.Minute),
		Depth:    envInt("DRIVELITE_GIT_DEPTH", 1),
		Username: env("DRIVELITE_GIT_USERNAME", ""),
		Token:    env("DRIVELITE_GIT_TOKEN", ""),
		WorkDir:  filepath.Join(c.CacheDir, "git"),
	}
	if c.Git.URL == "" {
		return fmt.Errorf("DRIVELITE_GIT_URL is required for the git backend")
	}
	if c.Git.Interval > 0 && c.Git.Interval < time.Minute {
		return fmt.Errorf("DRIVELITE_GIT_INTERVAL %s is too aggressive; use at least 1m (or 0 to disable refreshing)",
			c.Git.Interval)
	}
	return nil
}

func (c *Config) loadS3() error {
	c.S3 = S3Config{
		Endpoint:  env("DRIVELITE_S3_ENDPOINT", ""),
		Bucket:    env("DRIVELITE_S3_BUCKET", ""),
		Prefix:    env("DRIVELITE_S3_PREFIX", ""),
		Region:    env("DRIVELITE_S3_REGION", "us-east-1"),
		AccessKey: env("DRIVELITE_S3_ACCESS_KEY", ""),
		SecretKey: env("DRIVELITE_S3_SECRET_KEY", ""),
		Token:     env("DRIVELITE_S3_SESSION_TOKEN", ""),
		// Path style is the default because it is what every self-hosted
		// implementation (MinIO, Ceph, Garage) expects.
		PathStyle: envBool("DRIVELITE_S3_PATH_STYLE", true),
		CacheTTL:  envDuration("DRIVELITE_S3_CACHE_TTL", time.Minute),
		Timeout:   envDuration("DRIVELITE_S3_TIMEOUT", 60*time.Second),
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("DRIVELITE_S3_BUCKET is required for the s3 backend")
	}
	if c.S3.Endpoint == "" {
		return fmt.Errorf("DRIVELITE_S3_ENDPOINT is required for the s3 backend " +
			"(for AWS use https://s3.<region>.amazonaws.com)")
	}
	// Anonymous access to a public bucket is valid, but a half-filled pair
	// is always a mistake.
	if (c.S3.AccessKey == "") != (c.S3.SecretKey == "") {
		return fmt.Errorf("set both DRIVELITE_S3_ACCESS_KEY and DRIVELITE_S3_SECRET_KEY, or neither")
	}
	return nil
}

func (c *Config) loadAuth() error {
	var err error
	if raw := os.Getenv("DRIVELITE_USERS"); strings.TrimSpace(raw) != "" {
		if c.Users, err = parseUsers(raw); err != nil {
			return err
		}
	} else if pw := strings.TrimSpace(os.Getenv("DRIVELITE_PASSWORD")); pw != "" {
		c.Users = map[string]string{env("DRIVELITE_USER", "admin"): pw}
	} else {
		c.Users = map[string]string{}
	}

	if len(c.Users) == 0 && !c.Anonymous {
		return fmt.Errorf("no credentials configured: set DRIVELITE_PASSWORD (or DRIVELITE_USERS), " +
			"or set DRIVELITE_ALLOW_ANONYMOUS=true to deliberately serve without a login")
	}

	if key := strings.TrimSpace(os.Getenv("DRIVELITE_SESSION_KEY")); key != "" {
		c.Session = []byte(key)
	} else {
		// Ephemeral key: sessions simply do not survive a restart.
		c.Session = make([]byte, 32)
		if _, err := rand.Read(c.Session); err != nil {
			return fmt.Errorf("generating session key: %w", err)
		}
	}
	return nil
}
