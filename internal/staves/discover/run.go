package discover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ophymx/apt-signpost/internal/cooper/stage"
	"github.com/ophymx/apt-signpost/internal/staves/config"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// Options configures one Run invocation.
type Options struct {
	Tool plan.Tool
	Now  func() time.Time

	// PackageFilter restricts processing to packages whose nfpm
	// name is in the set. nil means "all packages."
	PackageFilter map[string]bool
}

// Run is the staves discover orchestrator. Walks every package in
// top, packs every local file into aux_files, derives source
// provenance from git, and emits a plan.Plan compatible with
// `cooper build -`.
func Run(ctx context.Context, top *config.Top, opts Options) (*plan.Plan, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	out := &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          opts.Tool,
		DiscoveredAt:  now().UTC().Format(time.RFC3339),
	}
	paths := append([]string(nil), top.Packages...)
	sort.Strings(paths)
	for _, dir := range paths {
		pkg := processPackage(ctx, opts, dir)
		if opts.PackageFilter != nil && !opts.PackageFilter[pkg.Name] {
			continue
		}
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

func processPackage(ctx context.Context, opts Options, dir string) plan.Package {
	pkg, err := config.LoadPackage(dir)
	if err != nil {
		return errPkg(filepathBase(dir), plan.ErrorKindDiscoveryFailed, err)
	}
	if err := config.RejectAssetishSrcs(config.CollectSrcPaths(&pkg.Nfpm)); err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindAuxResolutionFailed, err)
	}

	refs, err := stage.Walk(pkg.Dir, &pkg.Nfpm)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindAuxResolutionFailed, err)
	}

	commit, when, err := GitProvenance(ctx, pkg.Dir)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("staves needs a stable source_date_epoch — package must be in a git working tree with a commit touching it: %w", err))
	}
	sourceDateEpoch := when.Unix()

	nfpmClone, err := stage.CloneNfpm(&pkg.Nfpm)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed, fmt.Errorf("clone nfpm: %w", err))
	}
	// Staves recipes have no ${VERSION}/${ARCH} substitutions (the
	// user pins both literally in nfpm.yaml), so we skip
	// stage.SubstituteNfpm entirely. The plan still carries the
	// resolved version in plan.Artifact.Deb.Filename so drayman /
	// cooper-build downstream see the same string.
	nfpmJSON, err := stage.NfpmToJSON(nfpmClone)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed, fmt.Errorf("encode nfpm: %w", err))
	}

	vars := stage.Vars{
		Name:        pkg.NfpmName,
		Version:     pkg.Version,
		Arch:        pkg.Arch,
		Epoch:       0,
		PublishedAt: when.Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindAuxResolutionFailed, err)
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: sourceDateEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}
	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, nil, bp)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed, fmt.Errorf("compute build_inputs_hash: %w", err))
	}

	artifact := plan.Artifact{
		Arch:      pkg.Arch,
		Asset:     plan.Asset{}, // no upstream; cooper-build sees URL=="" and skips download
		Deb:       plan.Deb{Filename: fmt.Sprintf("%s_%s_%s.deb", pkg.NfpmName, pkg.Version, pkg.Arch), BuildInputsHash: hash},
		BuildPlan: bp,
	}
	return plan.Package{
		Name:   pkg.NfpmName,
		Result: plan.ResultOK,
		Source: &plan.Source{
			Kind:      plan.SourceKindLocal,
			GitCommit: commit,
			GitDate:   when.Format(time.RFC3339),
		},
		Artifacts: []plan.Artifact{artifact},
	}
}

func errPkg(name, kind string, err error) plan.Package {
	return plan.Package{
		Name:   name,
		Result: plan.ResultError,
		Error:  &plan.Error{Kind: kind, Message: err.Error()},
	}
}

// filepathBase is a minimal "basename" used as a fallback name when
// LoadPackage fails before we know the nfpm name. Avoids the
// path/filepath import for one call.
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
