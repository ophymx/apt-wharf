package policy

import (
	"context"
	"fmt"
	"sort"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Retention policy ("--retain N").
//
// Per (Package, Architecture), drayman keeps the N newest *upstream*
// versions and, within each kept upstream version, only its newest
// debian-revision. Older revisions of the same upstream are by
// construction rebuilds with stale inputs (recipe edit, asset
// mutation), so they carry no rollback value; counting upstream
// versions instead of full Debian versions means a flapping upstream
// asset can't crowd out genuinely older releases.
//
// Scope is deliberately narrow:
//
//   - Only entries carrying X-Cooper-Build-Inputs-Hash are considered.
//     Foreign entries (manual uploads, other producers) are neither
//     counted toward N nor removed.
//   - The entry whose hash matches keepHash (the artifact this run
//     imported or confirmed present) is never removed, even if an
//     oddly-revisioned sibling sorts above it.
//   - The newest entry of each kept upstream version is never removed,
//     so with N >= 1 the repo's current maximum is always safe.
//
// Callers decide *which* (Package, Architecture) pairs to apply this
// to; see cmd/drayman's reconcile loop, which only prunes pairs that
// settled this run (hash already present, or built and imported OK).
//
// `drayman import` handles packages built outside cooper, which carry
// no hash at all. For the pairs it is handed it uses the *All
// variants below: every entry of the pair is a candidate, and the
// run's own versions are protected by version instead of by hash.
// The hash-only scope above is unchanged for reconcile.

// Prune codes for Prune.Code.
const (
	// CodePruneSupersededRevision: an older debian-revision of an
	// upstream version that is being kept.
	CodePruneSupersededRevision = "superseded_revision"
	// CodePruneBeyondRetain: every entry of an upstream version that
	// falls outside the N newest.
	CodePruneBeyondRetain = "beyond_retain"
)

// Prune names one repo entry the retain policy removes.
type Prune struct {
	PackageName string
	Arch        string
	Entry       backend.Package // the entry to pass to Backend.Remove
	Code        string          // see CodePrune* constants
	Reason      string          // human-readable rationale
}

// PlanPrunes applies the retain policy to one (Package, Architecture)
// listing and returns the entries to remove, oldest first. retain <= 0
// disables the policy (returns nil). keepHash may be empty.
//
// Pure function: no I/O. PrunesFor wraps it with the backend query.
func PlanPrunes(existing []backend.Package, retain int, keepHash string) []Prune {
	return planPrunes(existing, retain,
		func(p backend.Package) bool { return p.BuildInputsHash != "" },
		func(p backend.Package) bool { return keepHash != "" && p.BuildInputsHash == keepHash })
}

// PlanPrunesAll is PlanPrunes for a pair whose entries carry no
// X-Cooper-Build-Inputs-Hash (prebuilt packages brought in by
// `drayman import`): every entry is a candidate, hashed or not, and
// entries at any of keepVersions (compared as dpkg compares them) are
// never removed. The keep-N-upstreams / newest-revision rule is the
// same, so a rebuild at "4.2-repack3" supersedes "4.2-repack2".
//
// Only hand this a pair the caller owns outright: unlike PlanPrunes
// it will remove entries drayman did not put there.
func PlanPrunesAll(existing []backend.Package, retain int, keepVersions []string) []Prune {
	return planPrunes(existing, retain,
		func(backend.Package) bool { return true },
		func(p backend.Package) bool {
			for _, v := range keepVersions {
				if plan.CompareVersions(p.Version, v) == 0 {
					return true
				}
			}
			return false
		})
}

// planPrunes is the shared retention core. candidate selects which
// entries count toward N and may be removed; keep vetoes the removal
// of an entry the rule would otherwise drop.
func planPrunes(existing []backend.Package, retain int, candidate, keep func(backend.Package) bool) []Prune {
	if retain <= 0 || len(existing) == 0 {
		return nil
	}

	// Candidates, deduplicated. reprepro lists an arch:all entry
	// once per binary-<arch> index; one Remove covers all of them.
	type key struct{ version, ref string }
	seen := map[key]bool{}
	var cands []backend.Package
	for _, p := range existing {
		if !candidate(p) {
			continue
		}
		k := key{p.Version, p.Ref}
		if seen[k] {
			continue
		}
		seen[k] = true
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return nil
	}

	// Group by upstream version; newest full version first within
	// each group.
	groups := map[string][]backend.Package{}
	for _, p := range cands {
		up, _ := plan.SplitDebianRevision(p.Version)
		groups[up] = append(groups[up], p)
	}
	upstreams := make([]string, 0, len(groups))
	for up, g := range groups {
		sort.SliceStable(g, func(i, j int) bool {
			return plan.CompareVersions(g[i].Version, g[j].Version) > 0
		})
		upstreams = append(upstreams, up)
	}
	sort.SliceStable(upstreams, func(i, j int) bool {
		return plan.CompareVersions(upstreams[i], upstreams[j]) > 0
	})

	var out []Prune
	for rank, up := range upstreams {
		g := groups[up]
		for i, p := range g {
			pr := Prune{PackageName: p.Name, Arch: p.Architecture, Entry: p}
			switch {
			case rank >= retain:
				pr.Code = CodePruneBeyondRetain
				pr.Reason = fmt.Sprintf("upstream %s ranks #%d newest; retain=%d", up, rank+1, retain)
			case i > 0:
				pr.Code = CodePruneSupersededRevision
				pr.Reason = fmt.Sprintf("superseded by %s (same upstream version)", g[0].Version)
			default:
				continue
			}
			if keep(p) {
				continue
			}
			out = append(out, pr)
		}
	}
	// Oldest first so a partial failure leaves the newest history intact.
	sort.SliceStable(out, func(i, j int) bool {
		return plan.CompareVersions(out[i].Entry.Version, out[j].Entry.Version) < 0
	})
	return out
}

// PrunesFor lists (name, arch) from q, appends extra (entries the
// caller expects to be present but that aren't in the repo yet —
// used by peek/dry-run to predict the post-import outcome), and
// returns PlanPrunes over the union.
//
// Entries whose Name or Architecture differ from the requested pair
// are dropped before planning. Backends are expected to return exact
// matches, but removal is destructive and a loose listing (aptly's
// dependency query also answers with Provides-only and arch:all
// packages) must never let one pair's retention delete another's
// entries.
func PrunesFor(ctx context.Context, q backend.Querier, name, arch string, retain int, keepHash string, extra ...backend.Package) ([]Prune, error) {
	if retain <= 0 {
		return nil, nil
	}
	all, err := listPair(ctx, q, name, arch, extra)
	if err != nil {
		return nil, err
	}
	return PlanPrunes(all, retain, keepHash), nil
}

// PrunesForAll is PrunesFor over PlanPrunesAll: the hashless-inclusive
// policy `drayman import` applies to the pairs it was handed.
func PrunesForAll(ctx context.Context, q backend.Querier, name, arch string, retain int, keepVersions []string, extra ...backend.Package) ([]Prune, error) {
	if retain <= 0 {
		return nil, nil
	}
	all, err := listPair(ctx, q, name, arch, extra)
	if err != nil {
		return nil, err
	}
	return PlanPrunesAll(all, retain, keepVersions), nil
}

// listPair is the listing step behind PrunesFor and PrunesForAll: the
// repo's entries for exactly (name, arch), plus extra.
func listPair(ctx context.Context, q backend.Querier, name, arch string, extra []backend.Package) ([]backend.Package, error) {
	existing, err := q.ListByNameArch(ctx, name, arch)
	if err != nil {
		return nil, fmt.Errorf("%s %s: list by name/arch: %w", name, arch, err)
	}
	all := make([]backend.Package, 0, len(existing)+len(extra))
	for _, p := range existing {
		if p.Name == name && p.Architecture == arch {
			all = append(all, p)
		}
	}
	all = append(all, extra...)
	return all, nil
}

// PlannedVersion is the full Debian version a build decision will
// publish at: bare when Revision == 0, "<bare>-<N>" otherwise.
func PlannedVersion(d Decision) string {
	if d.Revision > 0 {
		return fmt.Sprintf("%s-%d", d.PlanVersion, d.Revision)
	}
	return d.PlanVersion
}
