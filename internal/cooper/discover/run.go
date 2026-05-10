package discover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
	cooperSrc "github.com/ophymx/apt-signpost/internal/cooper/source"
	"github.com/ophymx/apt-signpost/internal/cooper/stage"
	"github.com/ophymx/apt-signpost/internal/cooper/version"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// Options configures one Run invocation. Tool/Now are factored out so
// tests can pin deterministic values; callers supply a real *github.Client.
type Options struct {
	Tool   plan.Tool
	Client *github.Client
	Now    func() time.Time

	// PackageFilter restricts processing to packages whose nfpm name is
	// in the set. nil means "all packages."
	PackageFilter map[string]bool
}

// Run is the discover orchestrator. It produces a complete plan.Plan
// document covering every package in top (or every package whose name
// passes Options.PackageFilter).
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

	// Process packages in deterministic order.
	paths := append([]string(nil), top.Packages...)
	sort.Strings(paths)

	for _, path := range paths {
		pkg := processPackage(ctx, opts, path)
		if opts.PackageFilter != nil && !opts.PackageFilter[pkg.Name] {
			continue
		}
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

// processPackage runs the per-package pipeline. Any sub-step failure
// collapses the whole package into a result:"error" entry — orchestrators
// drop those before piping to build, and `cooper build` exits non-zero
// if any artifact ultimately fails.
func processPackage(ctx context.Context, opts Options, path string) plan.Package {
	pkgFile, err := config.LoadPackage(path)
	if err != nil {
		return plan.Package{
			Name:   pkgNameFromPath(path),
			Result: plan.ResultError,
			Error: &plan.Error{
				Kind:    plan.ErrorKindDiscoveryFailed,
				Message: err.Error(),
			},
		}
	}

	// Walk aux refs once for the package — they're shared across arches.
	refs, err := stage.Walk(pkgFile.Dir, &pkgFile.Nfpm)
	if err != nil {
		return errPkg(pkgFile.NfpmName, plan.ErrorKindAuxResolutionFailed, err)
	}

	release, err := cooperSrc.ResolveRelease(ctx, opts.Client, pkgFile.Sidecar.Source.GitHub)
	if err != nil {
		return errPkg(pkgFile.NfpmName, plan.ErrorKindDiscoveryFailed, err)
	}

	resolvedVersion, err := version.Assemble(&pkgFile.Sidecar, release)
	if err != nil {
		return errPkg(pkgFile.NfpmName, plan.ErrorKindVersionInvalid, err)
	}

	source := &plan.Source{
		Kind:               plan.SourceKindGitHubRelease,
		Repo:               pkgFile.Sidecar.Source.GitHub.Repo,
		ReleaseID:          release.GetID(),
		ReleaseTag:         release.GetTagName(),
		ReleasePublishedAt: release.GetPublishedAt().UTC().Format(time.RFC3339),
	}
	sourceDateEpoch := release.GetPublishedAt().Unix()

	archNames := sortedKeys(pkgFile.Sidecar.Arches)
	artifacts := make([]plan.Artifact, 0, len(archNames))
	for _, arch := range archNames {
		art, err := buildArtifact(opts, pkgFile, release, resolvedVersion, arch, sourceDateEpoch, refs)
		if err != nil {
			return errPkg(pkgFile.NfpmName, errKindFor(err), err)
		}
		artifacts = append(artifacts, art)
	}

	return plan.Package{
		Name:      pkgFile.NfpmName,
		Result:    plan.ResultOK,
		Source:    source,
		Artifacts: artifacts,
	}
}

// buildArtifact resolves one (package × arch) combination into a
// plan.Artifact. Failures here turn into per-package errors at the
// caller — there is no notion of partial success within a package.
func buildArtifact(
	opts Options,
	pkgFile *config.PackageFile,
	release *github.RepositoryRelease,
	resolvedVersion, arch string,
	sourceDateEpoch int64,
	refs []stage.Ref,
) (plan.Artifact, error) {
	archCfg := pkgFile.Sidecar.Arches[arch]
	asset, err := cooperSrc.MatchAsset(release, archCfg.Asset, resolvedVersion)
	if err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}

	// Build the per-arch nfpm subtree.
	nfpmClone, err := stage.CloneNfpm(&pkgFile.Nfpm)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("clone nfpm: %w", err)
	}
	stage.SubstituteNfpm(nfpmClone, resolvedVersion, arch)
	nfpmJSON, err := stage.NfpmToJSON(nfpmClone)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("encode nfpm: %w", err)
	}

	// Render aux files with real Vars (templates that error here are
	// genuine bugs since validate would have caught syntax/typo issues).
	vars := stage.Vars{
		Name:        pkgFile.NfpmName,
		Version:     resolvedVersion,
		Arch:        arch,
		Epoch:       pkgFile.Sidecar.Epoch,
		PublishedAt: release.GetPublishedAt().UTC().Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return plan.Artifact{}, &auxError{wrapped: err}
	}

	// Asset metadata.
	assetSHA := assetSHAPtr(asset)
	source := planSourceFromAsset(asset)
	planAsset := plan.Asset{
		Name:         asset.GetName(),
		URL:          asset.GetBrowserDownloadURL(),
		Size:         int64(asset.GetSize()),
		SHA256:       assetSHA,
		SHA256Source: source,
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: sourceDateEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}

	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, assetSHA, bp)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("compute build_inputs_hash: %w", err)
	}

	deb := plan.Deb{
		Filename:        fmt.Sprintf("%s_%s_%s.deb", pkgFile.NfpmName, debVersionString(resolvedVersion, pkgFile.Sidecar.Epoch), arch),
		BuildInputsHash: hash,
	}

	return plan.Artifact{
		Arch:      arch,
		Asset:     planAsset,
		Deb:       deb,
		BuildPlan: bp,
	}, nil
}

// debVersionString glues epoch onto the resolved upstream version per
// Debian's `[epoch:]upstream_version[-debian_revision]` format. Cooper
// has no Debian revision concept (rebuilds change input bytes via
// SOURCE_DATE_EPOCH, which is fixed by release.published_at), so the
// revision component is always absent.
func debVersionString(upstream string, epoch int) string {
	if epoch == 0 {
		return upstream
	}
	return fmt.Sprintf("%d:%s", epoch, upstream)
}

func assetSHAPtr(a *github.ReleaseAsset) *string {
	if hex, ok := cooperSrc.AssetSHA256(a); ok {
		s := "sha256:" + hex
		return &s
	}
	return nil
}

func planSourceFromAsset(a *github.ReleaseAsset) *string {
	if _, ok := cooperSrc.AssetSHA256(a); ok {
		s := plan.SHA256SourceGitHubAPI
		return &s
	}
	return nil
}

// errPkg builds a result:"error" plan.Package entry.
func errPkg(name, kind string, err error) plan.Package {
	return plan.Package{
		Name:   name,
		Result: plan.ResultError,
		Error: &plan.Error{
			Kind:    kind,
			Message: err.Error(),
		},
	}
}

// pkgNameFromPath is a fallback "name" when the package file failed to
// load and we don't know its nfpm name. The basename is friendlier than
// an empty string in plan output.
func pkgNameFromPath(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// discoveryError / auxError are sentinels so errKindFor can map a
// particular sub-step's failure to the right plan.Error.Kind without
// stringly-typed sniffing.
type discoveryError struct{ wrapped error }

func (d *discoveryError) Error() string { return d.wrapped.Error() }
func (d *discoveryError) Unwrap() error { return d.wrapped }

type auxError struct{ wrapped error }

func (a *auxError) Error() string { return a.wrapped.Error() }
func (a *auxError) Unwrap() error { return a.wrapped }

func errKindFor(err error) string {
	switch err.(type) {
	case *discoveryError:
		return plan.ErrorKindDiscoveryFailed
	case *auxError:
		return plan.ErrorKindAuxResolutionFailed
	default:
		return plan.ErrorKindDiscoveryFailed
	}
}
