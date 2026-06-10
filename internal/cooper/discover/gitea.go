package discover

import (
	"context"
	"fmt"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
	cooperSrc "github.com/ophymx/apt-wharf/internal/cooper/source"
	"github.com/ophymx/apt-wharf/internal/cooper/stage"
	"github.com/ophymx/apt-wharf/internal/cooper/version"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// processGiteaPackage is the gitea_release counterpart to
// processGitHubPackage. The two paths are intentionally parallel — same
// pipeline (release → version → per-(doc,arch) artifact), same error
// model — only the SDK and the plan.Source kind differ.
func processGiteaPackage(_ context.Context, opts Options, pkgFile *config.PackageFile, refsByDoc [][]stage.Ref) []plan.Package {
	if opts.GiteaClient == nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("source.gitea: discover.Options.GiteaClient is nil"))}
	}
	gt := pkgFile.Sidecar.Source.Gitea
	client, err := opts.GiteaClient(gt.Server, gt.TokenEnv, gt.TokenFile)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed,
			fmt.Errorf("source.gitea: build client for %s: %w", gt.Server, err))}
	}

	release, err := cooperSrc.ResolveGiteaRelease(client, gt)
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindDiscoveryFailed, err)}
	}

	resolvedVersion, err := version.Assemble(&pkgFile.Sidecar, version.FromGitea(release))
	if err != nil {
		return []plan.Package{errPkg(pkgFile.NfpmDocs[0].Name, plan.ErrorKindVersionInvalid, err)}
	}

	source := &plan.Source{
		Kind:               plan.SourceKindGiteaRelease,
		URL:                gt.Server,
		Repo:               gt.Repo,
		ReleaseID:          release.ID,
		ReleaseTag:         release.TagName,
		ReleasePublishedAt: release.PublishedAt.UTC().Format(time.RFC3339),
	}
	sourceDateEpoch := release.PublishedAt.Unix()
	archNames := sortedKeys(pkgFile.Sidecar.Arches)

	out := make([]plan.Package, 0, len(pkgFile.NfpmDocs))
	for i, doc := range pkgFile.NfpmDocs {
		artifacts := make([]plan.Artifact, 0, len(archNames))
		failed := false
		for _, arch := range archNames {
			art, err := buildGiteaArtifact(opts, pkgFile, &doc, refsByDoc[i], release, resolvedVersion, arch, sourceDateEpoch)
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

// buildGiteaArtifact resolves one (nfpm-doc × arch) combination into a
// plan.Artifact for a gitea_release recipe. Mirrors buildArtifact: assets
// come from the release attachments via exact name match (with
// ${VERSION} substitution); SHA256 is unset because Gitea's API doesn't
// expose attachment digests, so cooper-build will stream + hash at
// download time.
func buildGiteaArtifact(
	opts Options,
	pkgFile *config.PackageFile,
	doc *config.NfpmDoc,
	refs []stage.Ref,
	release *gitea.Release,
	resolvedVersion, arch string,
	sourceDateEpoch int64,
) (plan.Artifact, error) {
	archCfg := pkgFile.Sidecar.Arches[arch]
	assets, err := cooperSrc.MatchGiteaAssets(release, archCfg.Assets, resolvedVersion)
	if err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}

	nfpmClone, err := stage.CloneNfpm(&doc.Node)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("clone nfpm: %w", err)
	}
	if err := stage.SubstituteNfpm(nfpmClone, arch, stage.BuildSubs(resolvedVersion, arch, archCfg.Vars)); err != nil {
		return plan.Artifact{}, &discoveryError{wrapped: err}
	}
	nfpmJSON, err := stage.NfpmToJSON(nfpmClone)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("encode nfpm: %w", err)
	}

	vars := stage.Vars{
		Name:        doc.Name,
		Version:     resolvedVersion,
		Arch:        arch,
		Epoch:       pkgFile.Sidecar.Epoch,
		PublishedAt: release.PublishedAt.UTC().Format(time.RFC3339),
	}
	auxFiles, err := stage.MaterializeAuxFiles(refs, vars)
	if err != nil {
		return plan.Artifact{}, &auxError{wrapped: err}
	}

	planAssets := make([]plan.Asset, len(assets))
	for i, a := range assets {
		planAssets[i] = plan.Asset{
			Name: a.Name,
			URL:  a.DownloadURL,
			Size: a.Size,
			// SHA256 / SHA256Source unset — Gitea attachments do not
			// expose a digest. cooper-build streams and hashes; the
			// resulting SHA goes into the produced .deb's BuildPlan
			// upgrade path via the same plan.SHA256SourceHEADRequest
			// fallback the json_url path already uses.
		}
	}

	// Source archive: Gitea exposes the tag tarball as Release.TarURL
	// (https://<server>/<owner>/<repo>/archive/<tag>.tar.gz). Same
	// SHA256-unknown-at-discover treatment as the github_release path.
	var sourceArchive *plan.Asset
	if pkgFile.Sidecar.Source.Gitea.SourceArchive {
		sourceArchive = &plan.Asset{
			Name: release.TagName + ".tar.gz",
			URL:  release.TarURL,
		}
	}

	bp := plan.BuildPlan{
		SourceDateEpoch: sourceDateEpoch,
		Nfpm:            nfpmJSON,
		AuxFiles:        auxFiles,
	}

	tempArt := plan.Artifact{Assets: planAssets, SourceArchive: sourceArchive}
	hash, err := plan.ComputeBuildInputsHash(opts.Tool.FormatRevision, plan.ArtifactAssetSHAs(tempArt), bp)
	if err != nil {
		return plan.Artifact{}, fmt.Errorf("compute build_inputs_hash: %w", err)
	}

	deb := plan.Deb{
		Filename:        fmt.Sprintf("%s_%s_%s.deb", doc.Name, debVersionString(resolvedVersion, pkgFile.Sidecar.Epoch), arch),
		BuildInputsHash: hash,
	}

	return plan.Artifact{
		Arch:          arch,
		Assets:        planAssets,
		SourceArchive: sourceArchive,
		Deb:           deb,
		BuildPlan:     bp,
		Extract:       extractLimitsFromSidecar(pkgFile.Sidecar.Extract),
	}, nil
}
