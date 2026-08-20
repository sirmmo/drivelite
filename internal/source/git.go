package source

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GitOptions configures a git-backed source.
type GitOptions struct {
	URL      string // https://github.com/owner/repo.git, or a local path
	Ref      string // branch, tag or commit; empty means the remote default
	Subdir   string // optional: serve only this directory of the repo
	WorkDir  string // where the working tree is kept (outside the served tree)
	Interval time.Duration
	Depth    int // shallow clone depth; 0 for a full clone
	Username string
	Token    string
}

// Git serves a checked-out git working tree.
//
// The repository is cloned into a scratch directory and then served by the
// ordinary Local backend, so thumbnails, ZIP downloads and search all work
// exactly as they do for a plain folder. A background timer re-fetches on an
// interval; the working tree is only swapped under a write lock, so a request
// never observes a half-updated checkout.
type Git struct {
	opts    GitOptions
	dir     string
	display string // URL with any credentials removed, safe to log
	log     *slog.Logger

	mu    sync.RWMutex
	local *Local

	stop context.CancelFunc
	done chan struct{}

	stateMu  sync.Mutex
	head     string
	lastSync time.Time
	lastErr  error
}

// NewGit clones the repository and starts the refresh loop.
func NewGit(ctx context.Context, opts GitOptions, log *slog.Logger) (*Git, error) {
	if opts.URL == "" {
		return nil, errors.New("git URL is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("the git backend needs the git binary, which is not on PATH")
	}
	if opts.WorkDir == "" {
		return nil, errors.New("git work directory is required")
	}
	if opts.Depth < 0 {
		opts.Depth = 0
	}

	// One checkout directory per repository+ref, so switching configuration
	// does not reuse an unrelated tree.
	sum := sha256.Sum256([]byte(opts.URL + "\x00" + opts.Ref))
	dir := filepath.Join(opts.WorkDir, hex.EncodeToString(sum[:8]))

	g := &Git{
		opts:    opts,
		dir:     dir,
		display: sanitizeGitURL(opts.URL),
		log:     log,
		done:    make(chan struct{}),
	}

	if err := g.sync(ctx, true); err != nil {
		return nil, err
	}

	if opts.Interval > 0 {
		loopCtx, cancel := context.WithCancel(context.Background())
		g.stop = cancel
		go g.refreshLoop(loopCtx)
	} else {
		close(g.done)
	}
	return g, nil
}

// sanitizeGitURL strips any embedded credentials so the URL can be logged.
func sanitizeGitURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// authArgs returns the git -c flags that authenticate a network operation.
//
// The credential travels as a per-invocation HTTP header rather than being
// baked into the remote URL. Writing it into the URL would persist it in
// .git/config — inside the very tree being served — so a single missed access
// check would hand out the token. This way nothing secret is ever written to
// disk.
func (g *Git) authArgs() []string {
	if g.opts.Token == "" {
		return nil
	}
	u, err := url.Parse(g.opts.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		// SSH and local paths authenticate by other means; a header is
		// meaningless there.
		return nil
	}
	user := g.opts.Username
	if user == "" {
		// GitHub and GitLab both accept an arbitrary username with a token.
		user = "x-access-token"
	}
	credential := base64.StdEncoding.EncodeToString([]byte(user + ":" + g.opts.Token))
	return []string{"-c", "http.extraHeader=Authorization: Basic " + credential}
}

// authSecrets lists strings that must never appear in surfaced output.
func (g *Git) authSecrets() []string {
	if g.opts.Token == "" {
		return nil
	}
	secrets := []string{g.opts.Token}
	for _, arg := range g.authArgs() {
		if strings.HasPrefix(arg, "http.extraHeader=") {
			secrets = append(secrets, strings.TrimPrefix(arg, "http.extraHeader=Authorization: Basic "))
		}
	}
	return secrets
}

// run executes a git command, returning trimmed stdout.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	// Never let git block waiting for a password on a terminal that is not there.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"GCM_INTERACTIVE=never",
	)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// Credentials can appear in git's error output; scrub before surfacing.
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(redactArgs(args), " "), err, redactText(text, g.authSecrets()...))
	}
	return text, nil
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "http.extraHeader=") {
			out[i] = "http.extraHeader=***"
			continue
		}
		out[i] = sanitizeGitURL(a)
	}
	return out
}

func redactText(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// sync fetches the configured ref and updates the working tree.
func (g *Git) sync(ctx context.Context, initial bool) error {
	ref := g.opts.Ref

	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err != nil {
		if err := os.MkdirAll(g.dir, 0o755); err != nil {
			return fmt.Errorf("creating git work directory: %w", err)
		}
		if _, err := g.run(ctx, "-C", g.dir, "init", "--quiet"); err != nil {
			return err
		}
		if _, err := g.run(ctx, "-C", g.dir, "remote", "add", "origin", g.opts.URL); err != nil {
			return err
		}
	} else {
		// Refresh the remote URL in case the token was rotated.
		if _, err := g.run(ctx, "-C", g.dir, "remote", "set-url", "origin", g.opts.URL); err != nil {
			return err
		}
	}

	// With no ref configured, ask the remote what its default branch is.
	if ref == "" {
		out, err := g.run(ctx, append(g.authArgs(), "-C", g.dir, "remote", "show", "origin")...)
		if err != nil {
			return fmt.Errorf("determining default branch: %w", err)
		}
		ref = "HEAD"
		for _, line := range strings.Split(out, "\n") {
			if _, after, ok := strings.Cut(strings.TrimSpace(line), "HEAD branch: "); ok {
				ref = strings.TrimSpace(after)
				break
			}
		}
	}

	fetch := append(g.authArgs(), "-C", g.dir, "fetch", "--force", "--tags", "--prune")
	if g.opts.Depth > 0 {
		fetch = append(fetch, fmt.Sprintf("--depth=%d", g.opts.Depth))
	}
	fetch = append(fetch, "origin", ref)
	if _, err := g.run(ctx, fetch...); err != nil {
		return err
	}

	head, err := g.run(ctx, "-C", g.dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}

	g.stateMu.Lock()
	unchanged := head == g.head
	g.stateMu.Unlock()

	if unchanged && !initial {
		g.stateMu.Lock()
		g.lastSync = time.Now()
		g.lastErr = nil
		g.stateMu.Unlock()
		return nil
	}

	// Only the checkout mutates the working tree, so the exclusive lock is
	// held for as short a time as possible.
	g.mu.Lock()
	_, checkoutErr := g.run(ctx, "-C", g.dir, "checkout", "--detach", "--force", head)
	if checkoutErr == nil {
		_, checkoutErr = g.run(ctx, "-C", g.dir, "clean", "-fdx")
	}
	if checkoutErr == nil {
		root := g.dir
		if sub := Clean(g.opts.Subdir); sub != "" {
			root = filepath.Join(g.dir, filepath.FromSlash(sub))
		}
		var local *Local
		local, checkoutErr = NewLocal(root, g.labelFor(head))
		if checkoutErr == nil {
			g.local = local
		}
	}
	g.mu.Unlock()

	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	g.lastSync = time.Now()
	g.lastErr = checkoutErr
	if checkoutErr != nil {
		return checkoutErr
	}
	g.head = head

	short := head
	if len(short) > 8 {
		short = short[:8]
	}
	g.log.Info("git source updated", "repo", g.display, "ref", ref, "commit", short)
	return nil
}

func (g *Git) labelFor(head string) string {
	short := head
	if len(short) > 8 {
		short = short[:8]
	}
	ref := g.opts.Ref
	if ref == "" {
		ref = "default"
	}
	label := fmt.Sprintf("git %s@%s (%s)", g.display, ref, short)
	if sub := Clean(g.opts.Subdir); sub != "" {
		label += " /" + sub
	}
	return label
}

func (g *Git) refreshLoop(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(g.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if err := g.sync(runCtx, false); err != nil {
				// A failed refresh is not fatal: keep serving the last good tree.
				g.log.Warn("git refresh failed, serving the previous checkout",
					"repo", g.display, "err", err)
			}
			cancel()
		}
	}
}

// Status reports what is currently checked out, for logs and diagnostics.
func (g *Git) Status() (head string, lastSync time.Time, err error) {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	return g.head, g.lastSync, g.lastErr
}

// Name implements Source.
func (g *Git) Name() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.local == nil {
		return "git " + g.display
	}
	return g.local.Name()
}

// Close implements Source.
func (g *Git) Close() error {
	if g.stop != nil {
		g.stop()
	}
	<-g.done
	return nil
}

// List implements Source.
func (g *Git) List(ctx context.Context, dir string) ([]Entry, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.local == nil {
		return nil, ErrNotFound
	}
	return g.local.List(ctx, dir)
}

// Stat implements Source.
func (g *Git) Stat(ctx context.Context, name string) (Entry, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.local == nil {
		return Entry{}, ErrNotFound
	}
	return g.local.Stat(ctx, name)
}

// Walk implements Source.
func (g *Git) Walk(ctx context.Context, dir string, fn func(Entry) error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.local == nil {
		return ErrNotFound
	}
	return g.local.Walk(ctx, dir, fn)
}

// Open implements Source.
//
// The read lock is held until the returned file is closed, so a refresh
// cannot pull the working tree out from under an in-flight download. A very
// long download therefore delays the next refresh rather than corrupting it.
func (g *Git) Open(ctx context.Context, name string) (File, error) {
	g.mu.RLock()
	if g.local == nil {
		g.mu.RUnlock()
		return nil, ErrNotFound
	}
	f, err := g.local.Open(ctx, name)
	if err != nil {
		g.mu.RUnlock()
		return nil, err
	}
	return &gitFile{File: f, release: g.mu.RUnlock}, nil
}

type gitFile struct {
	File
	once    sync.Once
	release func()
}

func (f *gitFile) Close() error {
	err := f.File.Close()
	f.once.Do(f.release)
	return err
}
