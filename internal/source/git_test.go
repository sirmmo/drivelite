package source

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newUpstream creates a real repository on disk to clone from.
func newUpstream(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	git("init", "--quiet", "--initial-branch=main")
	mustWrite(t, filepath.Join(dir, "README.md"), "# demo repo")
	mustWrite(t, filepath.Join(dir, "photo.jpg"), "not really a jpeg")
	if err := os.MkdirAll(filepath.Join(dir, "assets", "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "assets", "img", "logo.png"), "png-bytes")
	mustWrite(t, filepath.Join(dir, "assets", "notes.txt"), "some notes")

	git("add", "-A")
	git("commit", "--quiet", "-m", "initial")
	return dir
}

func newTestGit(t *testing.T, upstream string, opts GitOptions) *Git {
	t.Helper()
	opts.URL = upstream
	if opts.WorkDir == "" {
		opts.WorkDir = t.TempDir()
	}
	g, err := NewGit(context.Background(), opts, discardLogger())
	if err != nil {
		t.Fatalf("NewGit: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestGitClonesAndServes(t *testing.T) {
	requireGit(t)
	g := newTestGit(t, newUpstream(t), GitOptions{Ref: "main"})
	ctx := context.Background()

	entries, err := g.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got := names(entries)
	sort.Strings(got)
	// .git is hidden like any other dotfile.
	want := []string{"README.md", "assets", "photo.jpg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listing = %v, want %v", got, want)
	}

	f, err := g.Open(ctx, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "# demo repo" {
		t.Errorf("file contents = %q", body)
	}
}

func TestGitHidesDotGit(t *testing.T) {
	requireGit(t)
	g := newTestGit(t, newUpstream(t), GitOptions{Ref: "main"})

	entries, err := g.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == ".git" {
			t.Error(".git was exposed in the listing")
		}
	}
	// Nor should it be reachable directly.
	if _, err := g.Stat(context.Background(), ".git/config"); err == nil {
		t.Error(".git/config was readable")
	}
}

func TestGitSubdir(t *testing.T) {
	requireGit(t)
	g := newTestGit(t, newUpstream(t), GitOptions{Ref: "main", Subdir: "assets"})
	ctx := context.Background()

	entries, err := g.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got := names(entries)
	sort.Strings(got)
	if strings.Join(got, ",") != "img,notes.txt" {
		t.Errorf("subdir listing = %v, want img,notes.txt", got)
	}

	// Nothing above the subdir is reachable.
	if _, err := g.Stat(ctx, "README.md"); err == nil {
		t.Error("a file outside the subdir was reachable")
	}
	if _, err := g.Stat(ctx, "../README.md"); err == nil {
		t.Error("traversal above the subdir succeeded")
	}
}

func TestGitWalk(t *testing.T) {
	requireGit(t)
	g := newTestGit(t, newUpstream(t), GitOptions{Ref: "main"})

	var seen []string
	err := g.Walk(context.Background(), "", func(e Entry) error {
		seen = append(seen, e.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)
	want := []string{"README.md", "assets/img/logo.png", "assets/notes.txt", "photo.jpg"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("walk = %v, want %v", seen, want)
	}
}

func TestGitRefreshPicksUpNewCommits(t *testing.T) {
	requireGit(t)
	upstream := newUpstream(t)
	g := newTestGit(t, upstream, GitOptions{Ref: "main"})
	ctx := context.Background()

	firstHead, _, err := g.Status()
	if err != nil {
		t.Fatal(err)
	}

	// Add a commit upstream, then force a sync.
	mustWrite(t, filepath.Join(upstream, "added.txt"), "new file")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "second"}} {
		cmd := exec.Command("git", append([]string{"-C", upstream}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := g.sync(ctx, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	secondHead, lastSync, err := g.Status()
	if err != nil {
		t.Fatal(err)
	}
	if secondHead == firstHead {
		t.Error("HEAD did not advance after a new upstream commit")
	}
	if lastSync.IsZero() {
		t.Error("lastSync was not recorded")
	}

	if _, err := g.Stat(ctx, "added.txt"); err != nil {
		t.Errorf("new file not visible after refresh: %v", err)
	}
}

func TestGitBadURLFails(t *testing.T) {
	requireGit(t)
	_, err := NewGit(context.Background(), GitOptions{
		URL:     filepath.Join(t.TempDir(), "does-not-exist"),
		Ref:     "main",
		WorkDir: t.TempDir(),
		Depth:   1,
	}, discardLogger())
	if err == nil {
		t.Fatal("expected a clone of a missing repository to fail")
	}
}

func TestSanitizeGitURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/repo.git":                  "https://github.com/owner/repo.git",
		"https://user:token@github.com/owner/repo.git":       "https://user@github.com/owner/repo.git",
		"https://x-access-token:ghp_secret@github.com/a.git": "https://x-access-token@github.com/a.git",
		"git@github.com:owner/repo.git":                      "git@github.com:owner/repo.git",
	}
	for in, want := range cases {
		if got := sanitizeGitURL(in); got != want {
			t.Errorf("sanitizeGitURL(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(sanitizeGitURL(in), "ghp_secret") {
			t.Errorf("sanitizeGitURL leaked a token for %q", in)
		}
	}
}

func TestGitAuthArgsUseAHeaderNotTheURL(t *testing.T) {
	g := &Git{opts: GitOptions{URL: "https://github.com/owner/repo.git", Token: "tok123"}}

	args := g.authArgs()
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("authArgs = %v, want a -c pair", args)
	}
	want := "http.extraHeader=Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:tok123"))
	if args[1] != want {
		t.Errorf("authArgs header = %q, want %q", args[1], want)
	}

	// The token must never appear in the URL git writes to .git/config.
	if strings.Contains(g.opts.URL, "tok123") {
		t.Error("the remote URL carries the token")
	}

	g.opts.Username = "alice"
	if !strings.Contains(g.authArgs()[1],
		base64.StdEncoding.EncodeToString([]byte("alice:tok123"))) {
		t.Error("a configured username was not used")
	}

	// SSH and local paths have nowhere to put an HTTP header.
	ssh := &Git{opts: GitOptions{URL: "git@github.com:owner/repo.git", Token: "tok"}}
	if got := ssh.authArgs(); got != nil {
		t.Errorf("ssh authArgs = %v, want nil", got)
	}
	none := &Git{opts: GitOptions{URL: "https://example.com/r.git"}}
	if got := none.authArgs(); got != nil {
		t.Errorf("authArgs with no token = %v, want nil", got)
	}
}

// Neither the raw token nor its base64 form may survive into logged output.
func TestGitRedactsCredentialsInErrors(t *testing.T) {
	g := &Git{opts: GitOptions{URL: "https://example.com/r.git", Token: "tok123"}}

	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok123"))
	text := "fatal: auth failed for tok123 (" + encoded + ")"
	got := redactText(text, g.authSecrets()...)

	if strings.Contains(got, "tok123") || strings.Contains(got, encoded) {
		t.Errorf("redactText leaked a credential: %q", got)
	}

	args := redactArgs(g.authArgs())
	for _, a := range args {
		if strings.Contains(a, encoded) {
			t.Errorf("redactArgs leaked the header: %q", a)
		}
	}
}

func TestRedactText(t *testing.T) {
	if got := redactText("fatal: bad token tok123 here", "tok123"); strings.Contains(got, "tok123") {
		t.Errorf("redactText left the token in: %q", got)
	}
	if got := redactText("no secrets", ""); got != "no secrets" {
		t.Errorf("redactText with no token changed the text: %q", got)
	}
}

func TestGitCloseIsIdempotent(t *testing.T) {
	requireGit(t)
	g := newTestGit(t, newUpstream(t), GitOptions{Ref: "main", Interval: 0})
	if err := g.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
}

func TestGitRefreshLoopStops(t *testing.T) {
	requireGit(t)
	upstream := newUpstream(t)
	g, err := NewGit(context.Background(), GitOptions{
		URL: upstream, Ref: "main", WorkDir: t.TempDir(),
		Interval: time.Hour, Depth: 1,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: the refresh loop is not stopping")
	}
}
