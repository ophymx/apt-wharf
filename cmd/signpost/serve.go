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

	"github.com/ophymx/apt-signpost/internal/config"
	"github.com/ophymx/apt-signpost/internal/refresh"
	"github.com/ophymx/apt-signpost/internal/server"
	"github.com/ophymx/apt-signpost/internal/status"
)

func cmdServe(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to YAML config")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "json", "log format: json|text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log = newLogger(*logLevel, *logFormat)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	wired, err := Wire(cfg, log)
	if err != nil {
		return err
	}
	defer wired.Zero()

	if err := wired.Store.EnsureDirs(); err != nil {
		return fmt.Errorf("state dirs: %w", err)
	}

	tracker := status.NewTracker()
	enabled := map[string]bool{}
	discoveryTypes := map[string]string{}
	for name, src := range cfg.Sources {
		enabled[name] = src.IsEnabled()
		discoveryTypes[name] = src.Discovery.Type
	}
	if states, err := wired.Store.LoadSources(); err == nil {
		tracker.Seed(states, enabled, discoveryTypes)
	} else {
		// Non-fatal — empty seed; the next tick will fill the tracker in.
		log.Warn("status seed: load sources failed", "err", err)
		tracker.Seed(nil, enabled, discoveryTypes)
	}
	if bs, err := wired.Store.LoadBootstrap(); err == nil {
		tracker.SeedBootstrap(bs)
	}

	holder := &refresh.Holder{}
	rf := refresh.New(refresh.Options{
		Cfg:         cfg,
		Signer:      wired.Signer,
		Store:       wired.Store,
		Fetcher:     wired.Fetcher,
		HTTPClient:  wired.HTTPClient,
		Discoverers: wired.Discoverers,
		Tracker:     tracker,
		Holder:      holder,
		Logger:      log,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := rf.SyncImportNew(ctx); err != nil {
		return fmt.Errorf("sync import: %w", err)
	}
	if err := rf.Refresh(ctx); err != nil {
		return fmt.Errorf("initial refresh: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           server.Handler(holder, tracker, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErrs := make(chan error, 1)
	go func() {
		log.Info("HTTP listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrs <- err
		}
		close(serveErrs)
	}()

	go rf.Loop(ctx)

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-serveErrs:
		if err != nil {
			cancel()
			return fmt.Errorf("http: %w", err)
		}
	}

	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	return nil
}

// quietWriter swallows uncaught panics in the http server (none expected;
// here as a safety net).
var _ = os.Stderr
