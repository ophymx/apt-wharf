package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sort"

	"github.com/ophymx/apt-signpost/internal/config"
	"github.com/ophymx/apt-signpost/internal/fetch"
	"github.com/ophymx/apt-signpost/internal/index"
	"github.com/ophymx/apt-signpost/internal/source"
)

func cmdCheck(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to YAML config")
	only := fs.String("source", "", "limit check to a single source by name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	wired, err := Wire(cfg, log)
	if err != nil {
		return err
	}
	defer wired.Zero()

	names := make([]string, 0, len(wired.Discoverers))
	for n := range wired.Discoverers {
		if *only != "" && n != *only {
			continue
		}
		names = append(names, n)
	}
	if *only != "" && len(names) == 0 {
		return fmt.Errorf("source %q not found in config (or disabled)", *only)
	}
	sort.Strings(names)

	ctx := context.Background()
	anyFail := false
	for _, name := range names {
		ok, msg := checkOne(ctx, wired, name)
		status := "ok"
		if !ok {
			status = "FAIL"
			anyFail = true
		}
		fmt.Printf("%-5s %-30s %s\n", status, name, msg)
	}
	if anyFail {
		return fmt.Errorf("one or more sources failed check")
	}
	return nil
}

func checkOne(ctx context.Context, w *Wired, name string) (bool, string) {
	d := w.Discoverers[name]
	res, err := d.Probe(ctx, source.ProbeInput{HTTPClient: w.HTTPClient})
	if err != nil {
		return false, fmt.Sprintf("probe: %v", err)
	}
	needHash := res.Probe.AssetDigest == ""
	fr, err := w.Fetcher.Fetch(ctx, fetch.Options{URL: res.Probe.URL, NeedHash: needHash})
	if err != nil {
		return false, fmt.Sprintf("fetch: %v", err)
	}
	fields, err := index.ExtractFields(fr.Control)
	if err != nil {
		return false, fmt.Sprintf("control parse: %v", err)
	}
	hashed := "ok"
	if needHash {
		hashed = "computed"
	} else if res.Probe.AssetDigest != "" && fr.SHA256 != "" && res.Probe.AssetDigest != fr.SHA256 {
		return false, fmt.Sprintf("digest mismatch: github=%s computed=%s", res.Probe.AssetDigest, fr.SHA256)
	}
	return true, fmt.Sprintf("%s %s %s  sha256=%s  size=%d",
		fields.Package, fields.Version, fields.Architecture, hashed, fr.Size)
}
