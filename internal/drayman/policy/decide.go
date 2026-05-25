// Package policy implements the per-artifact decision drayman makes
// against a cooper discover plan and the current state of a target
// aptly repository.
//
// Algorithm (cooper-design.md §"Orchestrator dedup & version policy"
// and §"Version monotonicity"):
//
//  1. Query the repo by X-Cooper-Build-Inputs-Hash. If found, the
//     artifact is already imported — Skip (CodeHashInRepo).
//  2. Compute the repo's max version per (Package, Architecture). If
//     the plan's base-version sorts strictly older than that max,
//     Skip (CodeVersionRegression). Default behavior is skip-and-log
//     so a transient discover anomaly (e.g. an upstream backport
//     re-pointing GitHub's "latest") doesn't fail the entire reconcile;
//     the audit log records the anomaly for inspection.
//  3. Otherwise, enumerate existing versions of (Package, Architecture)
//     and group by base-version. If the plan's base-version is absent
//     from the repo, publish at bare (CodeBuildNew, Revision = 0).
//     If it exists, find the maximum debian-revision N among matching
//     entries and publish at N+1 (CodeBuildBump).
//
// Bare (no debian-revision) counts as revision 0 for the (N+1) formula.
// Non-integer revisions (a recipe author baking their own "-nmu1"
// suffix, or a manual upload) are excluded from max computation;
// drayman will not collide with them on the integer track.
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Action names the per-artifact decision drayman would take.
type Action string

const (
	// ActionSkip means the artifact will not be (re)built or imported
	// this run. Code carries the reason category.
	ActionSkip Action = "skip"
	// ActionBuild means the artifact should be (re)built and
	// published. Revision determines the version: 0 = bare, >0 =
	// pass --revision N to cooper build.
	ActionBuild Action = "build"
)

// Code is the machine-readable reason a Decision took its Action.
// Distinct from Action because multiple skip reasons exist and
// downstream consumers (audit log, --strict mode, dashboards) need
// to distinguish them.
const (
	// CodeHashInRepo: skip because the exact build_inputs_hash is
	// already present in the target repo (idempotent re-run).
	CodeHashInRepo = "hash_in_repo"
	// CodeDiscoverFailed: skip because cooper discover marked the
	// package as Result=error; no artifact exists to import.
	CodeDiscoverFailed = "discover_failed"
	// CodeVersionRegression: skip because the plan's base-version
	// sorts strictly older than the repo's current max version for
	// (Package, Architecture). cooper-design.md §"Version monotonicity".
	CodeVersionRegression = "version_regression"
	// CodeBuildNew: build at bare version. (Package, base-version,
	// Architecture) absent from repo.
	CodeBuildNew = "new_at_bare"
	// CodeBuildBump: build with --revision N+1. Existing entries at
	// the same base-version drove the auto-bump.
	CodeBuildBump = "auto_bump"
)

// Decision is the per-artifact result of running Decide against a
// plan and a target apt repo's current state. The audit-context
// fields are populated based on Code; consumers should consult Code
// to know which fields are meaningful.
type Decision struct {
	PackageName string
	Arch        string
	Action      Action
	Code        string // see Code* constants
	Revision    int    // 0 means publish bare (no --revision); >0 means pass --revision N
	Reason      string // human-readable rationale, shown in `drayman peek` output

	// Audit context. Populated based on Code; see field-level comments.

	// PlanVersion is the artifact's bare version from the discover plan.
	// Set on CodeVersionRegression, CodeBuildNew, CodeBuildBump.
	PlanVersion string
	// PlanHash is the artifact's build_inputs_hash from the discover plan.
	// Set on CodeHashInRepo, CodeVersionRegression, CodeBuildNew, CodeBuildBump.
	PlanHash string
	// RepoMaxVersion is the highest version present in the repo for
	// (Package, Architecture). Set on CodeVersionRegression.
	RepoMaxVersion string
	// PriorVersion is the existing repo version that contributed
	// the maxRev used for auto-bump (typically same base, different
	// revision). Set on CodeBuildBump.
	PriorVersion string
	// PriorHash is the existing repo entry's X-Cooper-Build-Inputs-Hash
	// being bumped over. Set on CodeBuildBump when known.
	PriorHash string
}

// Decide walks every package in p and returns a Decision per artifact
// for OK packages, plus one synthetic Skip decision per discover-failed
// package (so `drayman peek` shows discover failures rather than
// silently dropping them). q is bound to a single target repo by
// construction (see backend.Backend); policy doesn't pass repo
// identity through method calls.
func Decide(ctx context.Context, q backend.Querier, p *plan.Plan) ([]Decision, error) {
	var out []Decision
	for _, pkg := range p.Packages {
		if pkg.Result != plan.ResultOK {
			out = append(out, discoverFailedDecision(pkg))
			continue
		}
		for _, art := range pkg.Artifacts {
			d, err := decideOne(ctx, q, pkg, art)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// discoverFailedDecision builds a synthetic Skip decision for a
// package whose discover phase failed. Arch is empty because discover
// never populated artifacts; the reason carries the kind+message so
// the operator sees what broke.
func discoverFailedDecision(pkg plan.Package) Decision {
	reason := "discover failed"
	if pkg.Error != nil {
		reason = fmt.Sprintf("discover failed (%s): %s", pkg.Error.Kind, pkg.Error.Message)
	}
	return Decision{
		PackageName: pkg.Name,
		Action:      ActionSkip,
		Code:        CodeDiscoverFailed,
		Reason:      reason,
	}
}

func decideOne(ctx context.Context, q backend.Querier, pkg plan.Package, art plan.Artifact) (Decision, error) {
	d := Decision{
		PackageName: pkg.Name,
		Arch:        art.Arch,
		PlanHash:    art.Deb.BuildInputsHash,
	}

	// Step 1: primary dedup by hash.
	exists, err := q.HashExists(ctx, art.Deb.BuildInputsHash)
	if err != nil {
		return d, fmt.Errorf("%s %s: query hash: %w", pkg.Name, art.Arch, err)
	}
	if exists {
		d.Action = ActionSkip
		d.Code = CodeHashInRepo
		d.Reason = "hash already in repo (idempotent)"
		return d, nil
	}

	bareVersion, err := planBaseVersion(art.BuildPlan.Nfpm)
	if err != nil {
		return d, fmt.Errorf("%s %s: parse plan version: %w", pkg.Name, art.Arch, err)
	}
	d.PlanVersion = bareVersion
	existing, err := q.ListByNameArch(ctx, pkg.Name, art.Arch)
	if err != nil {
		return d, fmt.Errorf("%s %s: list by name/arch: %w", pkg.Name, art.Arch, err)
	}

	// Step 2: version monotonicity. Per cooper-design §"Version
	// monotonicity," skip artifacts whose plan upstream-version sorts
	// strictly older than the repo's current max upstream for (Package,
	// Architecture). Compare *upstream* versions (sans debian-revision)
	// so a plan at "0.140.0" doesn't look older than a repo entry at
	// "0.140.0-10" — that's the auto-bump case, not a regression.
	// Default action is skip-and-log so a transient discover anomaly
	// (e.g. an upstream backport re-pointing GitHub's "latest") doesn't
	// fail the entire reconcile.
	repoMaxUpstream := ""
	repoMaxFull := ""
	for _, p := range existing {
		up, _ := plan.SplitDebianRevision(p.Version)
		if repoMaxUpstream == "" || plan.CompareVersions(up, repoMaxUpstream) > 0 {
			repoMaxUpstream = up
			repoMaxFull = p.Version
		}
	}
	if repoMaxUpstream != "" && plan.CompareVersions(bareVersion, repoMaxUpstream) < 0 {
		d.Action = ActionSkip
		d.Code = CodeVersionRegression
		d.RepoMaxVersion = repoMaxFull
		d.Reason = fmt.Sprintf("plan upstream version %s sorts older than repo max %s (per dpkg --compare-versions)", bareVersion, repoMaxFull)
		return d, nil
	}

	// Step 3: revision lookup by (Package, base-version, Architecture).
	maxRev := -1 // sentinel: "no matching entry"
	var priorPkg backend.Package
	for _, p := range existing {
		upstream, rev := plan.SplitDebianRevision(p.Version)
		if upstream != bareVersion {
			continue
		}
		if rev == "" {
			if maxRev < 0 {
				maxRev = 0
				priorPkg = p
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
			priorPkg = p
		}
	}

	d.Action = ActionBuild
	if maxRev < 0 {
		d.Revision = 0
		d.Code = CodeBuildNew
		d.Reason = "new (Package, base-version, Architecture); publish at bare version"
	} else {
		d.Revision = maxRev + 1
		d.Code = CodeBuildBump
		d.PriorVersion = priorPkg.Version
		d.PriorHash = priorPkg.BuildInputsHash
		d.Reason = fmt.Sprintf("existing (Package, base-version, Architecture) max revision = %d; auto-bump to %d", maxRev, maxRev+1)
	}
	return d, nil
}

// planBaseVersion extracts the bare version field from the JSON-encoded
// nfpm config in build_plan. Discover always emits this bare (no
// debian-revision); cooper build rejects re-fed annotated plans before
// drayman would reach this code, so a "-" here is a recipe author's
// problem, not drayman's.
//
// Returns an error when the version field is missing, non-string, or
// empty — those would silently misbehave downstream (the upstream-match
// loop would exclude every existing entry, and drayman would publish
// at bare on top of an existing repo state).
func planBaseVersion(raw json.RawMessage) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	raw2, ok := m["version"]
	if !ok {
		return "", fmt.Errorf("nfpm config missing 'version' field")
	}
	v, ok := raw2.(string)
	if !ok {
		return "", fmt.Errorf("nfpm config 'version' is %T, want string", raw2)
	}
	if v == "" {
		return "", fmt.Errorf("nfpm config 'version' is empty")
	}
	return v, nil
}
