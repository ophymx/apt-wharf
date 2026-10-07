package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ophymx/apt-wharf/internal/signpost/config"
	"github.com/ophymx/apt-wharf/internal/signpost/export"
	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
)

// stringList is a repeatable flag value.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// cmdExport runs one refresh tick (or an offline rebuild from state) and
// writes the resulting snapshot as a static site under --out, with
// redirect/header rules for each --target. It shares config, state dir,
// signing, and discoverers with `serve`; the only difference is that the
// snapshot lands on disk instead of behind an HTTP listener.
func cmdExport(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to YAML config")
	outDir := fs.String("out", "", "output directory for the static site (required)")
	var targetSpecs stringList
	fs.Var(&targetSpecs, "target", "static host target, repeatable or comma-separated: "+strings.Join(export.TargetNames(), ", "))
	offline := fs.Bool("offline", false, "compose from the state dir without any discovery or fetch")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "json", "log format: json|text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" {
		return fmt.Errorf("--out is required")
	}
	targets, err := export.ParseTargets(targetSpecs)
	if err != nil {
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

	holder := &refresh.Holder{}
	rf := refresh.New(refresh.Options{
		Cfg:         cfg,
		Signer:      wired.Signer,
		Store:       wired.Store,
		Fetcher:     wired.Fetcher,
		HTTPClient:  wired.HTTPClient,
		Discoverers: wired.Discoverers,
		Holder:      holder,
		Logger:      log,
	})

	ctx := context.Background()
	if *offline {
		if err := rf.Rebuild(); err != nil {
			return fmt.Errorf("offline rebuild: %w", err)
		}
	} else {
		if err := rf.SyncImportNew(ctx); err != nil {
			return fmt.Errorf("sync import: %w", err)
		}
		if err := rf.Refresh(ctx); err != nil {
			return fmt.Errorf("refresh: %w", err)
		}
	}
	snap := holder.Load()
	if snap == nil {
		return fmt.Errorf("refresh produced no snapshot")
	}

	res, err := export.Write(snap, export.Options{
		OutDir:     *outDir,
		Targets:    targets,
		PathPrefix: cfg.Repository.PathPrefix(),
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name())
	}
	log.Info("export complete",
		"out", *outDir,
		"targets", strings.Join(names, ","),
		"files", len(snap.Files),
		"redirects", len(snap.Redirects),
		"written", res.Written,
		"pruned", res.Pruned)
	fmt.Fprintf(os.Stdout, "exported %d files and %d redirects to %s (%s)\n",
		len(snap.Files), len(snap.Redirects), *outDir, strings.Join(names, ","))
	return nil
}
