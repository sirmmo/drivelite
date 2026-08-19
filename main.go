// Command drivelite serves a mounted directory as a browsable, downloadable
// gallery behind a minimal login.
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
	"syscall"
	"time"

	"drivelite/internal/auth"
	"drivelite/internal/config"
	"drivelite/internal/httpd"
	"drivelite/internal/thumbs"
	"drivelite/internal/vault"
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

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	cache, err := thumbs.NewCache(cfg.CacheDir, cfg.ThumbPx, cfg.ThumbJobs)
	if err != nil {
		log.Error("thumbnail cache unavailable", "err", err)
		os.Exit(1)
	}

	a := auth.New(cfg.Users, cfg.Session, cfg.SessionTTL, cfg.SecureCk, cfg.Anonymous)
	srv, err := httpd.New(cfg, vault.New(cfg.Root), a, cache, log)
	if err != nil {
		log.Error("server setup failed", "err", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
		// No WriteTimeout: large downloads and ZIP streams run arbitrarily long.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if cfg.Anonymous {
		log.Warn("authentication is disabled — every visitor can read the whole folder")
	}
	log.Info("drivelite ready",
		"version", version,
		"addr", cfg.Addr,
		"root", cfg.Root,
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
		log.Error("server stopped", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("forced shutdown", "err", err)
	}
}
