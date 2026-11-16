package policy

import (
	"context"
	"fmt"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Import policy (`drayman import`).
//
// Some packages can't come through cooper: the vendor ships an
// installer that a third-party script repacks into finished .debs,
// the build is not reproducible, and it is far too expensive to run
// just to learn it wasn't needed. Those .debs carry no
// X-Cooper-Build-Inputs-Hash, so the hash dedup Decide relies on has
// nothing to match.
//
// For them the identity is the control file's (Package, Version,
// Architecture): a triple the repo already lists is skipped, anything
// else is imported exactly as built. There is no auto-bump (drayman
// can't rebuild the package at a new revision) and no way to replace
// a version in place — a same-version re-import would swap in
// different bytes under a version clients may already have installed.

// ActionImport means the prebuilt .deb will be imported as-is.
const ActionImport Action = "import"

const (
	// CodeVersionInRepo: skip because the repo already lists this
	// exact (Package, Version, Architecture).
	CodeVersionInRepo = "version_in_repo"
	// CodeImportPrebuilt: import; the repo does not list this
	// (Package, Version, Architecture).
	CodeImportPrebuilt = "prebuilt"
)

// Deb is one prebuilt .deb, identified by its control file.
type Deb struct {
	Path    string // where the file is, for Backend.Import
	Name    string // Package
	Version string // full Debian version, as built
	Arch    string // Architecture
}

// DecideImports returns one Decision per deb, in order: skip when the
// repo already lists its (Package, Version, Architecture) — versions
// compared as dpkg compares them — import otherwise. PlanVersion
// carries the .deb's version; the hash fields stay empty.
func DecideImports(ctx context.Context, q backend.Querier, debs []Deb) ([]Decision, error) {
	out := make([]Decision, 0, len(debs))
	for _, deb := range debs {
		existing, err := q.ListByNameArch(ctx, deb.Name, deb.Arch)
		if err != nil {
			return nil, fmt.Errorf("%s %s: list by name/arch: %w", deb.Name, deb.Arch, err)
		}
		d := Decision{
			PackageName: deb.Name,
			Arch:        deb.Arch,
			PlanVersion: deb.Version,
			Action:      ActionImport,
			Code:        CodeImportPrebuilt,
			Reason:      fmt.Sprintf("%s not in repo; import as built", deb.Version),
		}
		if HasVersion(existing, deb.Version) {
			d.Action = ActionSkip
			d.Code = CodeVersionInRepo
			d.Reason = fmt.Sprintf("%s already in repo", deb.Version)
		}
		out = append(out, d)
	}
	return out, nil
}

// HasVersion reports whether any entry sits at version, compared as
// dpkg compares versions ("1.0" and "0:1.0" are the same).
func HasVersion(entries []backend.Package, version string) bool {
	for _, p := range entries {
		if plan.CompareVersions(p.Version, version) == 0 {
			return true
		}
	}
	return false
}
