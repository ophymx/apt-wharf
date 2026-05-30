package build

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ophymx/apt-wharf/pkg/plan"
)

// Options configures one Run invocation.
type Options struct {
	OutDir    string
	WorkDir   string
	StageOnly bool
	KeepWork  bool

	// Revision, when > 0, appends "-N" to every artifact's version at
	// nfpm.yaml-write time. Rejects plans whose nfpm.version already
	// contains "-" (a recipe-baked debian-revision conflicts with the
	// orchestrator-supplied one). The build_inputs_hash is NOT
	// recomputed; see cooper-design.md §"`--revision N` mechanics".
	Revision int

	// Downloader and NfpmExec are dependencies tests can override.
	// Defaults: NewDownloader(http.DefaultClient) and DefaultNfpmExec.
	Downloader *Downloader
	NfpmExec   NfpmExecutor
}

// Run is the build orchestrator. It returns a re-emitted plan annotated
// with deb.path / deb.sha256 on every artifact that built successfully,
// and result:"error" + error.kind=build_failed on any artifact whose
// build pipeline raised.
//
// Per-artifact failures are isolated: a failed `foo amd64` does not
// block `bar amd64` (cooper-design.md §"Phases · Build").
func Run(ctx context.Context, p *plan.Plan, opts Options) (*plan.Plan, error) {
	if opts.OutDir == "" {
		opts.OutDir = "./dist"
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "./.cooper-work"
	}
	// nfpm runs with cmd.Dir set to the staging directory, so any
	// relative -t path or ${ASSETS} expansion would resolve against
	// staging instead of the caller's cwd. Absolutize once here so
	// everything downstream (debPath, assetDir → ASSETS env) stays
	// rooted at the caller's cwd.
	absOut, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("out-dir: %w", err)
	}
	opts.OutDir = absOut
	absWork, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("work-dir: %w", err)
	}
	opts.WorkDir = absWork
	if opts.StageOnly && opts.KeepWork {
		return nil, fmt.Errorf("--stage-only and --keep-work are mutually exclusive")
	}
	if opts.Revision < 0 {
		return nil, fmt.Errorf("--revision must be a positive integer, got %d", opts.Revision)
	}
	if err := rejectAnnotatedPlan(p); err != nil {
		return nil, err
	}
	if opts.Revision > 0 {
		if err := validateRevisionable(p); err != nil {
			return nil, err
		}
	}
	if opts.Downloader == nil {
		opts.Downloader = NewDownloader(http.DefaultClient)
	}
	if opts.NfpmExec == nil {
		opts.NfpmExec = DefaultNfpmExec
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("out-dir: %w", err)
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("work-dir: %w", err)
	}

	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(opts.WorkDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("run-dir: %w", err)
	}
	cleanup := !opts.KeepWork && !opts.StageOnly
	defer func() {
		if cleanup {
			_ = os.RemoveAll(runDir)
		}
	}()

	// Deep-ish copy of Packages: we mutate Artifacts.
	out := *p
	out.Packages = make([]plan.Package, len(p.Packages))
	for i, pkg := range p.Packages {
		if pkg.Result != plan.ResultOK {
			out.Packages[i] = pkg
			continue
		}
		newPkg := pkg
		newPkg.Artifacts = make([]plan.Artifact, len(pkg.Artifacts))
		for j, art := range pkg.Artifacts {
			built, ferr := buildOne(ctx, runDir, pkg, art, opts, p.Tool)
			if ferr != nil {
				art.Result = plan.ResultError
				art.Error = &plan.Error{
					Kind:    plan.ErrorKindBuildFailed,
					Message: ferr.Error(),
				}
				newPkg.Artifacts[j] = art
				continue
			}
			newPkg.Artifacts[j] = built
		}
		out.Packages[i] = newPkg
	}
	return &out, nil
}

// buildOne is the per-artifact pipeline. Everything that can fail
// returns through this single error path so the orchestrator can
// uniformly tag the artifact with build_failed.
func buildOne(
	ctx context.Context,
	runDir string,
	pkg plan.Package,
	art plan.Artifact,
	opts Options,
	tool plan.Tool,
) (plan.Artifact, error) {
	// Defense against tampered plans.
	ok, recomputed, err := plan.VerifyBuildInputsHash(tool, art)
	if err != nil {
		return art, fmt.Errorf("verify build_inputs_hash: %w", err)
	}
	if !ok {
		return art, fmt.Errorf("build_inputs_hash mismatch: stored=%s recomputed=%s", art.Deb.BuildInputsHash, recomputed)
	}

	if err := RejectNonZeroOwners(art.BuildPlan); err != nil {
		return art, err
	}

	// --revision rewrites the version (and therefore the filename and
	// VERSION env) at on-disk staging time only. Hash is unchanged.
	if opts.Revision > 0 {
		art.Deb.Filename = reviseFilename(art.Deb.Filename, art.Arch, opts.Revision)
	}

	staging := filepath.Join(runDir, pkg.Name, art.Arch)
	assetDir := filepath.Join(staging, "asset")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return art, fmt.Errorf("staging: %w", err)
	}

	// sourceDir hosts the extracted source_archive when the artifact
	// carries one; absolute path is plumbed into the SOURCE env var
	// for nfpm. Created up-front so the var has a stable target even
	// for archive-only recipes whose first reference is a glob.
	sourceDir := ""
	if art.SourceArchive != nil {
		sourceDir = filepath.Join(staging, "source")
		if err := os.MkdirAll(sourceDir, 0o755); err != nil {
			return art, fmt.Errorf("source staging: %w", err)
		}
	}

	// Asset-optional path: local-source producers (staves) and any
	// future tool that bakes all bytes into aux_files emit plans with
	// an empty Assets slice. Skip download/extract entirely; the
	// aux_files step below carries every file the .deb needs.
	//
	// For each asset: download, verify SHA256 (or record it when the
	// plan didn't carry one), and extract in place if the file is an
	// archive. The asset_*[i] iteration is deterministic — order comes
	// from the recipe and from MatchAssets/RenderAssetURLs preserving
	// it through discover.
	for i := range art.Assets {
		a := &art.Assets[i]
		if a.URL == "" {
			continue
		}
		assetPath := filepath.Join(assetDir, a.Name)
		gotSHA, _, err := opts.Downloader.Fetch(ctx, a.URL, assetPath, a.Size)
		if err != nil {
			return art, fmt.Errorf("download %s: %w", a.URL, err)
		}
		if err := VerifySHA256(a.SHA256, gotSHA); err != nil {
			return art, err
		}
		// If the plan didn't carry a SHA256, record it now per
		// cooper-design.md §"Discover JSON contract" ("build streams +
		// hashes; orchestrator should treat such artifacts as re-import
		// unconditionally").
		if a.SHA256 == nil {
			s := "sha256:" + gotSHA
			a.SHA256 = &s
			src := plan.SHA256SourceHEADRequest
			a.SHA256Source = &src
		}

		if IsArchive(a.Name) {
			if err := Extract(assetPath, assetDir, extractOptsFromArtifact(art)); err != nil {
				return art, fmt.Errorf("extract %s: %w", a.Name, err)
			}
			if err := os.Remove(assetPath); err != nil {
				return art, fmt.Errorf("remove archive: %w", err)
			}
		}
	}

	// Source archive (source.github.source_archive: true). Always an
	// archive; download → verify-or-record SHA → extract into sourceDir.
	// Per-arch redundant downloads are tolerated for v1 — same archive
	// fetched once per arch. Add a download cache in Options if/when the
	// bandwidth becomes a real problem.
	if art.SourceArchive != nil {
		sa := art.SourceArchive
		archivePath := filepath.Join(sourceDir, sa.Name)
		gotSHA, _, err := opts.Downloader.Fetch(ctx, sa.URL, archivePath, sa.Size)
		if err != nil {
			return art, fmt.Errorf("download source_archive %s: %w", sa.URL, err)
		}
		if err := VerifySHA256(sa.SHA256, gotSHA); err != nil {
			return art, fmt.Errorf("source_archive: %w", err)
		}
		if sa.SHA256 == nil {
			s := "sha256:" + gotSHA
			sa.SHA256 = &s
			src := plan.SHA256SourceHEADRequest
			sa.SHA256Source = &src
		}
		if err := Extract(archivePath, sourceDir, extractOptsFromArtifact(art)); err != nil {
			return art, fmt.Errorf("extract source_archive %s: %w", sa.Name, err)
		}
		if err := os.Remove(archivePath); err != nil {
			return art, fmt.Errorf("remove source archive: %w", err)
		}
	}

	if err := WriteAuxFiles(art.BuildPlan.AuxFiles, staging); err != nil {
		return art, err
	}
	if err := WriteNfpmYAML(art.BuildPlan, NfpmYAMLOpts{
		Hash:     art.Deb.BuildInputsHash,
		Revision: opts.Revision,
	}, filepath.Join(staging, "nfpm.yaml")); err != nil {
		return art, err
	}
	if err := PinMtimes(staging, art.BuildPlan.SourceDateEpoch); err != nil {
		return art, fmt.Errorf("pin mtimes: %w", err)
	}

	if opts.StageOnly {
		// Skip nfpm exec; leave the work dir intact for inspection.
		return art, nil
	}

	debPath := filepath.Join(opts.OutDir, art.Deb.Filename)
	env := BuildEnv(versionFromArtifact(pkg.Name, art), art.Arch, assetDir, sourceDir, art.BuildPlan.SourceDateEpoch)
	if err := opts.NfpmExec(ctx, staging, debPath, env); err != nil {
		return art, err
	}

	debSHA, err := hashFile(debPath)
	if err != nil {
		return art, fmt.Errorf("hash deb: %w", err)
	}
	pathCopy := debPath
	shaCopy := "sha256:" + debSHA
	art.Deb.Path = &pathCopy
	art.Deb.SHA256 = &shaCopy
	return art, nil
}

// rejectAnnotatedPlan refuses to build a plan whose artifacts already
// carry a populated deb.path — that's the annotated output of a prior
// cooper build run, not a fresh discover plan. Re-feeding the
// annotated form would risk double-applying --revision and otherwise
// confuses the build pipeline; orchestrators should pipe discover
// output (where deb.path is nil) into build, not vice versa.
func rejectAnnotatedPlan(p *plan.Plan) error {
	for _, pkg := range p.Packages {
		for _, art := range pkg.Artifacts {
			if art.Deb.Path != nil && *art.Deb.Path != "" {
				return fmt.Errorf("%s %s: plan looks like cooper build's annotated output (deb.path=%q); pipe a fresh cooper discover plan instead",
					pkg.Name, art.Arch, *art.Deb.Path)
			}
		}
	}
	return nil
}

// validateRevisionable scans every artifact in p for a recipe-baked
// debian-revision and rejects the plan when --revision is in play. The
// debian-revision slot belongs to cooper under --revision; a recipe
// that already populates it conflicts with the orchestrator-supplied
// one.
//
// Two ways a recipe can populate the slot:
//
//  1. `version:` contains a "-" (nfpm parses that as a debian-revision
//     when version_schema=none, or as a semver prerelease otherwise —
//     either way it's not cooper's slot to claim).
//  2. `release:` is set to a non-empty value (nfpm's dedicated
//     debian-revision field).
func validateRevisionable(p *plan.Plan) error {
	for _, pkg := range p.Packages {
		for _, art := range pkg.Artifacts {
			v, release, err := nfpmVersionAndRelease(art.BuildPlan.Nfpm)
			if err != nil {
				return fmt.Errorf("%s %s: %w", pkg.Name, art.Arch, err)
			}
			if strings.Contains(v, "-") {
				return fmt.Errorf("--revision incompatible with %s %s: nfpm.version=%q already contains a debian-revision; drop --revision or fix the recipe's version_template",
					pkg.Name, art.Arch, v)
			}
			if release != "" {
				return fmt.Errorf("--revision incompatible with %s %s: nfpm.release=%q is set; drop --revision or remove `release:` from the recipe",
					pkg.Name, art.Arch, release)
			}
		}
	}
	return nil
}

// nfpmVersionAndRelease extracts the version and release fields from a
// build_plan.nfpm RawMessage. Missing or non-string fields return "".
// The only error path is JSON decode failure.
func nfpmVersionAndRelease(raw json.RawMessage) (version, release string, err error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", fmt.Errorf("decode nfpm json: %w", err)
	}
	version, _ = m["version"].(string)
	release, _ = m["release"].(string)
	return version, release, nil
}

// reviseFilename splices "-<revision>" into a <name>_<version>_<arch>.deb
// filename, between the version and the "_<arch>.deb" suffix. No-op
// when revision <= 0 or the filename doesn't match the expected shape.
func reviseFilename(filename, arch string, revision int) string {
	if revision <= 0 {
		return filename
	}
	suffix := "_" + arch + ".deb"
	if !strings.HasSuffix(filename, suffix) {
		return filename
	}
	base := filename[:len(filename)-len(suffix)]
	return fmt.Sprintf("%s-%d%s", base, revision, suffix)
}

// versionFromArtifact pulls the upstream version out of art.Deb.Filename,
// which discover constructed as <name>_<version>_<arch>.deb. Debian's
// version grammar excludes underscore, so the split is unambiguous.
func versionFromArtifact(pkgName string, art plan.Artifact) string {
	name := strings.TrimSuffix(art.Deb.Filename, ".deb")
	prefix := pkgName + "_"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := name[len(prefix):]
	suffix := "_" + art.Arch
	if !strings.HasSuffix(rest, suffix) {
		return ""
	}
	return rest[:len(rest)-len(suffix)]
}

// newRunID returns 16 hex chars from /dev/urandom-equivalent. The ID
// never appears in the .deb (per cooper-design.md), so it does not
// affect reproducibility.
func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EmitJSON marshals p to indented JSON suitable for stdout or `-o
// FILE`. Kept here so cmd/cooper can use the same shape build chose.
func EmitJSON(p *plan.Plan) ([]byte, error) {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// AnyArtifactFailed reports whether any artifact in the re-emitted plan
// carries result:"error". The CLI uses this to set process exit code.
func AnyArtifactFailed(p *plan.Plan) bool {
	for _, pkg := range p.Packages {
		if pkg.Result == plan.ResultError {
			return true
		}
		for _, art := range pkg.Artifacts {
			if art.Result == plan.ResultError {
				return true
			}
		}
	}
	return false
}

// extractOptsFromArtifact translates the plan-side ExtractLimits into
// the build-side ExtractOpts. nil and zero fields fall through; the
// Extract function handles the "≤ 0 → default" substitution itself.
func extractOptsFromArtifact(art plan.Artifact) ExtractOpts {
	if art.Extract == nil {
		return ExtractOpts{}
	}
	return ExtractOpts{
		MaxBytes: art.Extract.MaxBytes,
		MaxFiles: art.Extract.MaxFiles,
	}
}
