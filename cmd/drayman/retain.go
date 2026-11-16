package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/internal/drayman/policy"
)

// registerRetainFlag binds --retain, shared by peek, reconcile, and import.
// 0 (the default) leaves every version in place — today's behavior.
func registerRetainFlag(fs *flag.FlagSet) *int {
	return fs.Int("retain", 0, "keep only the N newest upstream versions per (package, arch), newest revision each; 0 = keep everything")
}

// validateRetain rejects a negative --retain and, for reprepro
// backends, prints a one-line reminder that reprepro has its own
// version limit which runs first (one version per package on 5.3;
// `Limit:` in conf/distributions on 5.4+). Not an error: drayman's
// prune simply finds nothing beyond reprepro's own limit.
func validateRetain(retain int, backendKind string, stderr io.Writer) error {
	if retain < 0 {
		return fmt.Errorf("--retain must be >= 0, got %d", retain)
	}
	if retain > 0 && strings.HasPrefix(backendKind, "reprepro") {
		fmt.Fprintln(stderr, "note: reprepro enforces its own per-package version limit at include time (1 on reprepro 5.3; `Limit:` in conf/distributions on 5.4+); --retain only prunes beyond that")
	}
	return nil
}

// predictPrunes computes, for peek and --dry-run, what the retain
// policy would remove once each build decision lands: the repo's
// current listing plus a synthetic entry at the planned version.
// Only pairs that would settle (hash already present, or a build) are
// considered, mirroring the reconcile loop's gate.
func predictPrunes(ctx context.Context, q backend.Querier, decisions []policy.Decision, retain int) ([]policy.Prune, error) {
	if retain <= 0 {
		return nil, nil
	}
	var out []policy.Prune
	for _, d := range decisions {
		var extra []backend.Package
		switch {
		case d.Action == policy.ActionSkip && d.Code == policy.CodeHashInRepo:
		case d.Action == policy.ActionBuild:
			extra = append(extra, backend.Package{
				Name:            d.PackageName,
				Version:         policy.PlannedVersion(d),
				Architecture:    d.Arch,
				BuildInputsHash: d.PlanHash,
			})
		default:
			continue
		}
		prunes, err := policy.PrunesFor(ctx, q, d.PackageName, d.Arch, retain, d.PlanHash, extra...)
		if err != nil {
			return nil, fmt.Errorf("retain: %w", err)
		}
		out = append(out, prunes...)
	}
	return out, nil
}

// printPrunesTo writes one `<package>\t<arch>\tprune\t<version>: <reason>`
// line per prune, matching the decision row layout so peek output
// stays grep-friendly.
func printPrunesTo(w io.Writer, prunes []policy.Prune) {
	for _, p := range prunes {
		fmt.Fprintf(w, "%s\t%s\tprune\t%s: %s\n", p.PackageName, p.Arch, p.Entry.Version, p.Reason)
	}
}
