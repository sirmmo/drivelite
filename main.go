// Command drivelite serves a mounted folder, a git repository, or an
// S3-compatible bucket as a browsable, downloadable gallery behind a minimal
// login.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"drivelite/internal/auth"
	"drivelite/internal/config"
	"drivelite/internal/httpd"
	"drivelite/internal/source"
	"drivelite/internal/thumbs"
)

// version is overwritten at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("drivelite", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Give backend setup — a git clone in particular — a bounded window.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelStart()

	src, err := buildSource(startCtx, cfg, log)
	if err != nil {
		return err
	}
	defer src.Close()

	cache, err := thumbs.NewCache(filepath.Join(cfg.CacheDir, "thumbs"), cfg.ThumbPx, cfg.ThumbJobs)
	if err != nil {
		return fmt.Errorf("thumbnail cache unavailable: %w", err)
	}

	a := auth.New(cfg.Users, cfg.Session, cfg.SessionTTL, cfg.SecureCk, cfg.Anonymous)
	srv, err := httpd.New(cfg, src, a, cache, log)
	if err != nil {
		return fmt.Errorf("server setup failed: %w", err)
	}

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
		// No WriteTimeout: large downloads and ZIP streams run arbitrarily long.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if cfg.Anonymous {
		log.Warn("authentication is disabled — every visitor can read the whole tree")
	}
	log.Info("drivelite ready",
		"version", version,
		"addr", cfg.Addr,
		"backend", cfg.Backend,
		"source", src.Name(),
		"cache", cfg.CacheDir,
		"users", len(cfg.Users),
		"zip", cfg.EnableZip)

	// Shut down cleanly so in-flight downloads are not cut off abruptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("forced shutdown", "err", err)
	}
	return nil
}

// buildSource constructs the backend named by the configuration.
func buildSource(ctx context.Context, cfg *config.Config, log *slog.Logger) (source.Source, error) {
	switch cfg.Backend {
	case config.BackendLocal:
		return source.NewLocal(cfg.Root, "")

	case config.BackendGit:
		log.Info("cloning git source", "url", cfg.Git.URL, "ref", cfg.Git.Ref)
		return source.NewGit(ctx, source.GitOptions{
			URL:      cfg.Git.URL,
			Ref:      cfg.Git.Ref,
			Subdir:   cfg.Git.Subdir,
			WorkDir:  cfg.Git.WorkDir,
			Interval: cfg.Git.Interval,
			Depth:    cfg.Git.Depth,
			Username: cfg.Git.Username,
			Token:    cfg.Git.Token,
		}, log)

	case config.BackendS3:
		log.Info("connecting to S3", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket)
		return source.NewS3(ctx, source.S3Options{
			Endpoint:  cfg.S3.Endpoint,
			Bucket:    cfg.S3.Bucket,
			Prefix:    cfg.S3.Prefix,
			Region:    cfg.S3.Region,
			AccessKey: cfg.S3.AccessKey,
			SecretKey: cfg.S3.SecretKey,
			Token:     cfg.S3.Token,
			PathStyle: cfg.S3.PathStyle,
			CacheTTL:  cfg.S3.CacheTTL,
			Timeout:   cfg.S3.Timeout,
		})
	}
	return nil, fmt.Errorf("unknown backend %q", cfg.Backend)
}
