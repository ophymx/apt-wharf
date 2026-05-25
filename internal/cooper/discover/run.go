package discover

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
	cooperSrc "github.com/ophymx/apt-wharf/internal/cooper/source"
	"github.com/ophymx/apt-wharf/internal/cooper/stage"
	"github.com/ophymx/apt-wharf/internal/cooper/version"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Options configures one Run invocation. Tool/Now are factored out so
// tests can pin deterministic values; callers supply a real *github.Client
// for source.github recipes and an http.Client (default
// http.DefaultClient) for source.json_url recipes.
type Options struct {
	Tool       plan.Tool
	Client     *github.Client
	HTTPClient *http.Client
	Now        func() time.Time

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
		pkgs := processPackage(ctx, opts, path)
		for _, pkg := range pkgs {
			if opts.PackageFilter != nil && !opts.PackageFilter[pkg.Name] {
				continue
			}
			out.Packages = append(out.Packages, pkg)
		}
	}
	return out, nil
}

// processPackage runs the per-package pipeline and emits one
// plan.Package per nfpm doc in the recipe (docs 2..N in the
// multi-doc YAML form). Source resolution, version resolution, and
// per-arch asset resolution happen ONCE per recipe and are shared
// across every emitted package; the nfpm doc + aux refs per package
// differ. Any sub-step failure collapses the whole recipe into a
// single result:"error" entry.
func processPackage(ctx context.Context, opts Options, path string) []plan.Package {
	pkgFile, err := config.LoadPackage(path)
	if err != nil {
		return []plan.Package{{
			Name:   pkgNameFromPath(path),
			Result: plan.ResultError,
			Error: &plan.Error{
				Kind:    plan.ErrorKindDiscoveryFailed,
				Message: err.Error(),
			},
		}}
	}

	// Walk aux refs PER nfpm doc — each doc has its own
	// contents[].src and scripts.{...} entries.
	refsByDoc := make([][]stage.Ref, len(pkgFile.NfpmDocs))
	for i, d := range pkgFile.NfpmDocs {
		refs, werr := stage.Walk(pkgFile.Dir, &d.Node)
		if werr != nil {
			return []plan.Package{errPkg(d.Name, plan.ErrorKindAuxResolutionFailed, werr)}
		}
		refsByDoc[i] = refs
	}

	switch {
	case pkgFile.Sidecar.Source.GitHub != nil:
		return processGitHubPackage(ctx, opts, pkgFile, refsByDoc)
	case pkgFile.Sidecar.Source.JSONURL != nil:
		return processJSONURLPackage(ctx, opts, pkgFile, refsByDoc)
	case pkgFile.Sidecar.Source.XMLURL != nil:
		return processXMLURLPackage(ctx, opts, pkgFile, refsByDoc)
	default:
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("source: no backend selected (validate should have caught this)"))}
	}
}

func processGitHubPackage(ctx context.Context, opts Options, pkgFile *config.PackageFile, refsByDoc [][]stage.Ref) []plan.Package {
	release, err := cooperSrc.ResolveRelease(ctx, opts.Client, pkgFile.Sidecar.Source.GitHub)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed, err)}
	}

	resolvedVersion, err := version.Assemble(&pkgFile.Sidecar, release)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindVersionInvalid, err)}
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

	// Emit one plan.Package per nfpm doc. Source + per-arch asset
	// resolution is shared; nfpm + aux + hash + filename are
	// per-doc.
	out := make([]plan.Package, 0, len(pkgFile.NfpmDocs))
	for i, doc := range pkgFile.NfpmDocs {
		artifacts := make([]plan.Artifact, 0, len(archNames))
		failed := false
		for _, arch := range archNames {
			art, err := buildArtifact(opts, pkgFile, &doc, refsByDoc[i], release, resolvedVersion, arch, sourceDateEpoch)
			if err != nil {
				out = append(out, errPkg(doc.Name, errKindFor(err), err))
				failed = true
				break
			}
			artifacts = append(artifacts, art)
		}
		if failed {
			continue
		}
		out = append(out, plan.Package{
			Name:      doc.Name,
			Result:    plan.ResultOK,
			Source:    source,
			Artifacts: artifacts,
		})
	}
	return out
}

// processJSONURLPackage is the json_url counterpart to
// processGitHubPackage. One HTTP GET resolves the version; per-arch
// asset URLs are rendered from arches[].asset_url against the same
// JSON body. source_date_epoch is derived deterministically from
// (url, version) since the JSON has no canonical "released at" field.
func processJSONURLPackage(ctx context.Context, opts Options, pkgFile *config.PackageFile, refsByDoc [][]stage.Ref) []plan.Package {
	resolution, err := cooperSrc.ResolveJSONURL(ctx, opts.HTTPClient, pkgFile.Sidecar.Source.JSONURL)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed, err)}
	}

	resolvedVersion := resolution.Version
	if !version.MatchesDebianGrammar(resolvedVersion) {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindVersionInvalid,
			fmt.Errorf("version %q (from %s) does not match Debian's grammar `^[0-9][A-Za-z0-9.+~-]*$`",
				resolvedVersion, pkgFile.Sidecar.Source.JSONURL.VersionPath))}
	}

	resolvedVersionWithStrip := resolvedVersion
	if pkgFile.Sidecar.Source.JSONURL.VersionStripPrefix != "" {
		// ResolveJSONURL already applied this; keep `resolvedVersion`
		// post-strip. This branch is left as a placeholder if we ever
		// expose the raw token separately.
	}
	_ = resolvedVersionWithStrip

	source := &plan.Source{
		Kind:  plan.SourceKindJSONURL,
		URL:   resolution.URL,
		Token: resolvedVersion,
	}

	archNames := sortedKeys(pkgFile.Sidecar.Arches)
	out := make([]plan.Package, 0, len(pkgFile.NfpmDocs))
	for i, doc := range pkgFile.NfpmDocs {
		artifacts := make([]plan.Artifact, 0, len(archNames))
		failed := false
		for _, arch := range archNames {
			art, err := buildJSONURLArtifact(opts, pkgFile, &doc, refsByDoc[i], resolution, resolvedVersion, arch)
			if err != nil {
				out = append(out, errPkg(doc.Name, errKindFor(err), err))
				failed = true
				break
			}
			artifacts = append(artifacts, art)
		}
		if failed {
			continue
		}
		out = append(out, plan.Package{
			Name:      doc.Name,
			Result:    plan.ResultOK,
			Source:    source,
			Artifacts: artifacts,
		})
	}
	return out
}

// processXMLURLPackage is the xml_url counterpart to
// processJSONURLPackage. Same shape (one HTTP GET, per-arch URL
// rendering against the body, derived source_date_epoch); only the
// query language differs — XPath via xmlquery instead of gjson.
func processXMLURLPackage(ctx context.Context, opts Options, pkgFile *config.PackageFile, refsByDoc [][]stage.Ref) []plan.Package {
	resolution, err := cooperSrc.ResolveXMLURL(ctx, opts.HTTPClient, pkgFile.Sidecar.Source.XMLURL)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed, err)}
	}

	resolvedVersion := resolution.Version
	if !version.MatchesDebianGrammar(resolvedVersion) {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindVersionInvalid,
			fmt.Errorf("version %q (from %s) does not match Debian's grammar `^[0-9][A-Za-z0-9.+~-]*$`",
				resolvedVersion, pkgFile.Sidecar.Source.XMLURL.VersionXPath))}
	}

	source := &plan.Source{
		Kind:  plan.SourceKindXMLURL,
		URL:   resolution.URL,
		Token: resolvedVersion,
	}

	archNames := sortedKeys(pkgFile.Sidecar.Arches)
	out := make([]plan.Package, 0, len(pkgFile.NfpmDocs))
	for i, doc := range pkgFile.NfpmDocs {
		artifacts := make([]plan.Artifact, 0, len(archNames))
		failed := false
		for _, arch := range archNames {
			art, err := buildXMLURLArtifact(opts, pkgFile, &doc, refsByDoc[i], resolution, resolvedVersion, arch)
			if err != nil {
				out = append(out, errPkg(doc.Name, errKindFor(err), err))
				failed = true
				break
			}
			artifacts = append(artifacts, art)
		}
		if failed {
			continue
		}
		out = append(out, plan.Package{
			Name:      doc.Name,
			Result:    plan.ResultOK,
			Source:    source,
			Artifacts: artifacts,
		})
	}
	return out
}

// buildXMLURLArtifact resolves one (nfpm-doc × arch) combination against
// an already-fetched XML resolution. Mirrors buildJSONURLArtifact; only
// the per-arch URL renderer differs (RenderXMLAssetURLs vs RenderAssetURLs).
func buildXMLURLArtifact(
	opts Options,
	pkgFile *config.PackageFile,
	doc *config.NfpmDoc,
	refs []stage.Ref,
	resolution *cooperSrc.XMLURLResolution,
	resolvedVersion, arch string,
) (plan.Artifact, error) {
	archCfg := pkgFile.Sidecar.Arches[arch]
	assetURLs, err := cooperSrc.RenderXMLAssetURLs(archCfg.AssetURLs, resolvedVersion, arch, resolution.RawBody)
	if err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}

	nfpmClone, err := stage.CloneNfpm(&doc.Node)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("clone nfpm: %w", err)
	}
	stage.SubstituteNfpm(nfpmClone, resolvedVersion, arch)
	nfpmJSON, err := stage.NfpmToJSON(nfpmClone)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("encode nfpm: %w", err)
	}

	vars := stage.Vars{
		Name:        doc.Name,
		Version:     resolvedVersion,
		Arch:        arch,
		Epoch:       pkgFile.Sidecar.Epoch,
		PublishedAt: time.Unix(resolution.SourceEpoch, 0).UTC().Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return plan.Artifact{}, &auxError{wrapped: err}
	}

	planAssets := make([]plan.Asset, len(assetURLs))
	assetSHAs := make([]*string, len(assetURLs))
	for i, u := range assetURLs {
		planAssets[i] = plan.Asset{
			Name: basenameFromURL(u),
			URL:  u,
		}
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: resolution.SourceEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}

	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, assetSHAs, bp)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("compute build_inputs_hash: %w", err)
	}

	deb := plan.Deb{
		Filename:        fmt.Sprintf("%s_%s_%s.deb", doc.Name, debVersionString(resolvedVersion, pkgFile.Sidecar.Epoch), arch),
		BuildInputsHash: hash,
	}

	return plan.Artifact{
		Arch:      arch,
		Assets:    planAssets,
		Deb:       deb,
		BuildPlan: bp,
	}, nil
}

// buildArtifact resolves one (nfpm-doc × arch) combination into a
// plan.Artifact. Failures here turn into per-package errors at the
// caller. Assets are shared across all nfpm docs in the recipe
// (resolved once per arch from the same release); nfpm subtree + aux
// files + hash + filename are per-doc.
func buildArtifact(
	opts Options,
	pkgFile *config.PackageFile,
	doc *config.NfpmDoc,
	refs []stage.Ref,
	release *github.RepositoryRelease,
	resolvedVersion, arch string,
	sourceDateEpoch int64,
) (plan.Artifact, error) {
	archCfg := pkgFile.Sidecar.Arches[arch]
	assets, err := cooperSrc.MatchAssets(release, archCfg.Assets, resolvedVersion)
	if err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}

	// Build the per-(doc, arch) nfpm subtree.
	nfpmClone, err := stage.CloneNfpm(&doc.Node)
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
		Name:        doc.Name,
		Version:     resolvedVersion,
		Arch:        arch,
		Epoch:       pkgFile.Sidecar.Epoch,
		PublishedAt: release.GetPublishedAt().UTC().Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return plan.Artifact{}, &auxError{wrapped: err}
	}

	// Asset metadata — one plan.Asset per resolved release asset.
	planAssets := make([]plan.Asset, len(assets))
	assetSHAs := make([]*string, len(assets))
	for i, a := range assets {
		sha := assetSHAPtr(a)
		planAssets[i] = plan.Asset{
			Name:         a.GetName(),
			URL:          a.GetBrowserDownloadURL(),
			Size:         int64(a.GetSize()),
			SHA256:       sha,
			SHA256Source: planSourceFromAsset(a),
		}
		assetSHAs[i] = sha
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: sourceDateEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}

	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, assetSHAs, bp)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("compute build_inputs_hash: %w", err)
	}

	deb := plan.Deb{
		Filename:        fmt.Sprintf("%s_%s_%s.deb", doc.Name, debVersionString(resolvedVersion, pkgFile.Sidecar.Epoch), arch),
		BuildInputsHash: hash,
	}

	return plan.Artifact{
		Arch:      arch,
		Assets:    planAssets,
		Deb:       deb,
		BuildPlan: bp,
	}, nil
}

// buildJSONURLArtifact resolves one (nfpm-doc × arch) combination
// against an already-fetched JSON resolution. The asset URL is
// rendered per-arch from the recipe template; size and SHA256 are
// unknown at discover time, so cooper-build will stream + hash during
// download. nfpm subtree + aux + hash + filename are per-doc.
func buildJSONURLArtifact(
	opts Options,
	pkgFile *config.PackageFile,
	doc *config.NfpmDoc,
	refs []stage.Ref,
	resolution *cooperSrc.JSONURLResolution,
	resolvedVersion, arch string,
) (plan.Artifact, error) {
	archCfg := pkgFile.Sidecar.Arches[arch]
	assetURLs, err := cooperSrc.RenderAssetURLs(archCfg.AssetURLs, resolvedVersion, arch, resolution.RawBody)
	if err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}

	// Build the per-(doc, arch) nfpm subtree.
	nfpmClone, err := stage.CloneNfpm(&doc.Node)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("clone nfpm: %w", err)
	}
	stage.SubstituteNfpm(nfpmClone, resolvedVersion, arch)
	nfpmJSON, err := stage.NfpmToJSON(nfpmClone)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("encode nfpm: %w", err)
	}

	vars := stage.Vars{
		Name:        doc.Name,
		Version:     resolvedVersion,
		Arch:        arch,
		Epoch:       pkgFile.Sidecar.Epoch,
		PublishedAt: time.Unix(resolution.SourceEpoch, 0).UTC().Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return plan.Artifact{}, &auxError{wrapped: err}
	}

	// Asset metadata: one plan.Asset per resolved URL. Size and SHA256
	// are unknown at discover time for json_url — build will stream +
	// hash and record. All shas are nil; that flows into the hash as a
	// slice of N null entries.
	planAssets := make([]plan.Asset, len(assetURLs))
	assetSHAs := make([]*string, len(assetURLs))
	for i, u := range assetURLs {
		planAssets[i] = plan.Asset{
			Name: basenameFromURL(u),
			URL:  u,
		}
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: resolution.SourceEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}

	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, assetSHAs, bp)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("compute build_inputs_hash: %w", err)
	}

	deb := plan.Deb{
		Filename:        fmt.Sprintf("%s_%s_%s.deb", doc.Name, debVersionString(resolvedVersion, pkgFile.Sidecar.Epoch), arch),
		BuildInputsHash: hash,
	}

	return plan.Artifact{
		Arch:      arch,
		Assets:    planAssets,
		Deb:       deb,
		BuildPlan: bp,
	}, nil
}

// basenameFromURL returns the last path segment of a URL, used as the
// asset.name for json_url-sourced artifacts. Strips query and fragment.
func basenameFromURL(raw string) string {
	end := len(raw)
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		end = i
	}
	return path.Base(raw[:end])
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
