package discover

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ophymx/apt-wharf/internal/cooper/stage"
	"github.com/ophymx/apt-wharf/internal/staves/config"
	"github.com/ophymx/apt-wharf/pkg/plan"
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
// top, packs every local file into aux_files, derives a content-hash
// source_date_epoch (git-independent), and emits a plan.Plan
// compatible with `cooper build -`.
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

	// Content-hash source_date_epoch. Hashes the nfpm subtree + raw
	// source bytes of every aux file (pre-template-render) into the
	// 2020–2030 epoch window via plan.DeriveEpoch. Same recipe → same
	// epoch on every host, no git dependency. Template-rendered bytes
	// are excluded from the hash here because they depend on
	// PublishedAt, which depends on this epoch (cycle); only the
	// .tmpl source content goes in.
	sourceDateEpoch, err := computeContentEpoch(nfpmJSON, refs)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed, err)
	}

	// Best-effort git provenance. Populated when the package lives in
	// a git working tree with a commit touching it; left empty
	// otherwise (no-git operators and shallow CI checkouts both
	// fall through cleanly because SDE no longer depends on this).
	commit, gitWhen, _ := GitProvenance(ctx, pkg.Dir)

	vars := stage.Vars{
		Name:        pkg.NfpmName,
		Version:     pkg.Version,
		Arch:        pkg.Arch,
		Epoch:       0,
		PublishedAt: time.Unix(sourceDateEpoch, 0).UTC().Format(time.RFC3339),
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
	// Staves never has upstream assets — every byte the .deb needs is
	// in aux_files. Empty Assets slice + nil-shas hash input is the
	// canonical asset-optional shape.
	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, nil, bp)
	if err != nil {
		return errPkg(pkg.NfpmName, plan.ErrorKindDiscoveryFailed, fmt.Errorf("compute build_inputs_hash: %w", err))
	}

	artifact := plan.Artifact{
		Arch:      pkg.Arch,
		Assets:    nil, // no upstream; cooper-build skips download when Assets is empty
		Deb:       plan.Deb{Filename: fmt.Sprintf("%s_%s_%s.deb", pkg.NfpmName, pkg.Version, pkg.Arch), BuildInputsHash: hash},
		BuildPlan: bp,
	}
	src := &plan.Source{Kind: plan.SourceKindLocal}
	if commit != "" {
		src.GitCommit = commit
		src.GitDate = gitWhen.Format(time.RFC3339)
	}
	return plan.Package{
		Name:      pkg.NfpmName,
		Result:    plan.ResultOK,
		Source:    src,
		Artifacts: []plan.Artifact{artifact},
	}
}

// computeContentEpoch derives source_date_epoch deterministically
// from the recipe's source bytes. The input is the canonical nfpm
// JSON plus, for each aux ref (sorted by Key), the raw on-disk
// source bytes. Template renders are NOT folded in here because the
// renderer needs PublishedAt = time.Unix(SDE), which would create a
// cycle; the source .tmpl content covers the same change-detection
// surface anyway.
func computeContentEpoch(nfpmJSON []byte, refs []stage.Ref) (int64, error) {
	h := sha256.New()
	h.Write([]byte("nfpm:"))
	h.Write(nfpmJSON)
	h.Write([]byte{0})

	sorted := append([]stage.Ref(nil), refs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for _, r := range sorted {
		body, err := os.ReadFile(r.AbsPath)
		if err != nil {
			return 0, fmt.Errorf("read %s for content-epoch: %w", r.AbsPath, err)
		}
		h.Write([]byte("aux:"))
		h.Write([]byte(r.Key))
		h.Write([]byte{0})
		h.Write(body)
		h.Write([]byte{0})
	}
	return plan.DeriveEpoch(h.Sum(nil)), nil
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
