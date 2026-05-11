// Package policy implements the per-artifact decision drayman makes
// against a cooper discover plan and the current state of a target
// aptly repository.
//
// Algorithm (cooper-design.md §"Orchestrator dedup & version policy"):
//
//  1. Query aptly by X-Cooper-Build-Inputs-Hash. If found, the
//     artifact is already imported — Skip.
//  2. Otherwise, enumerate existing versions of (Package, Architecture)
//     in the repo and group by base-version. If the plan's base-version
//     is absent from the repo, publish at bare (Revision = 0).
//     If it exists, find the maximum debian-revision N among matching
//     entries and publish at N+1.
//
// Bare (no debian-revision) counts as revision 0 for the (N+1) formula,
// so publishing on top of a bare-only (P, base-V, A) yields -1.
// Non-integer revisions (a recipe author baking their own "-nmu1"
// suffix, or a manual upload) are excluded from max computation;
// drayman will not collide with them on the integer track.
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ophymx/apt-signpost/internal/drayman/aptly"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// Action names the per-artifact decision drayman would take.
type Action string

const (
	// ActionSkip means the artifact's hash already exists in the
	// target repo — re-importing would be a no-op.
	ActionSkip Action = "skip"
	// ActionBuild means the artifact should be (re)built and
	// published. Revision determines the version: 0 = bare, >0 =
	// pass --revision N to cooper build.
	ActionBuild Action = "build"
)

// Decision is the per-artifact result of running Decide against a
// plan and a target apt repo's current state.
type Decision struct {
	PackageName string
	Arch        string
	Action      Action
	Revision    int    // 0 means publish bare (no --revision); >0 means pass --revision N
	Reason      string // human-readable rationale, shown in `drayman peek` output
}

// Querier is the subset of the aptly client that policy needs. Defined
// here (rather than importing aptly.Client) so tests can stub it out
// without spinning up an httptest server for every case.
type Querier interface {
	HashExists(ctx context.Context, repo, hash string) (bool, error)
	ListByNameArch(ctx context.Context, repo, name, arch string) ([]aptly.Package, error)
}

// Decide walks every artifact in p with Result=="ok" and returns a
// Decision per artifact. Errors short-circuit and surface the
// (Package, Architecture) that failed.
func Decide(ctx context.Context, q Querier, repo string, p *plan.Plan) ([]Decision, error) {
	var out []Decision
	for _, pkg := range p.Packages {
		if pkg.Result != plan.ResultOK {
			continue
		}
		for _, art := range pkg.Artifacts {
			d, err := decideOne(ctx, q, repo, pkg, art)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func decideOne(ctx context.Context, q Querier, repo string, pkg plan.Package, art plan.Artifact) (Decision, error) {
	d := Decision{PackageName: pkg.Name, Arch: art.Arch}

	// Step 1: primary dedup by hash.
	exists, err := q.HashExists(ctx, repo, art.Deb.BuildInputsHash)
	if err != nil {
		return d, fmt.Errorf("%s %s: query hash: %w", pkg.Name, art.Arch, err)
	}
	if exists {
		d.Action = ActionSkip
		d.Reason = "hash already in repo (idempotent)"
		return d, nil
	}

	// Step 2: revision lookup by (Package, base-version, Architecture).
	bareVersion, err := planBaseVersion(art.BuildPlan.Nfpm)
	if err != nil {
		return d, fmt.Errorf("%s %s: parse plan version: %w", pkg.Name, art.Arch, err)
	}
	existing, err := q.ListByNameArch(ctx, repo, pkg.Name, art.Arch)
	if err != nil {
		return d, fmt.Errorf("%s %s: list by name/arch: %w", pkg.Name, art.Arch, err)
	}

	maxRev := -1 // sentinel: "no matching entry"
	for _, p := range existing {
		upstream, rev := plan.SplitDebianRevision(p.Version)
		if upstream != bareVersion {
			continue
		}
		if rev == "" {
			if 0 > maxRev {
				maxRev = 0
			}
			continue
		}
		n, err := strconv.Atoi(rev)
		if err != nil {
			// Non-integer revision (recipe-baked oddity or manual
			// upload). Skip from max computation; drayman never
			// collides with it on the integer track.
			continue
		}
		if n > maxRev {
			maxRev = n
		}
	}

	d.Action = ActionBuild
	if maxRev < 0 {
		d.Revision = 0
		d.Reason = "new (Package, base-version, Architecture); publish at bare version"
	} else {
		d.Revision = maxRev + 1
		d.Reason = fmt.Sprintf("existing (Package, base-version, Architecture) max revision = %d; auto-bump to %d", maxRev, maxRev+1)
	}
	return d, nil
}

// planBaseVersion extracts the bare version field from the JSON-encoded
// nfpm config in build_plan. Discover always emits this bare (no
// debian-revision); cooper build rejects re-fed annotated plans before
// drayman would reach this code, so a "-" here is a recipe author's
// problem, not drayman's.
func planBaseVersion(raw json.RawMessage) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	v, _ := m["version"].(string)
	return v, nil
}
