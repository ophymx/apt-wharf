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

	"github.com/ophymx/apt-signpost/pkg/plan"
)

// Options configures one Run invocation.
type Options struct {
	OutDir    string
	WorkDir   string
	StageOnly bool
	KeepWork  bool

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
	if opts.StageOnly && opts.KeepWork {
		return nil, fmt.Errorf("--stage-only and --keep-work are mutually exclusive")
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

	staging := filepath.Join(runDir, pkg.Name, art.Arch)
	assetDir := filepath.Join(staging, "asset")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return art, fmt.Errorf("staging: %w", err)
	}

	assetPath := filepath.Join(assetDir, art.Asset.Name)
	gotSHA, _, err := opts.Downloader.Fetch(ctx, art.Asset.URL, assetPath, art.Asset.Size)
	if err != nil {
		return art, fmt.Errorf("download %s: %w", art.Asset.URL, err)
	}
	if err := VerifySHA256(art.Asset.SHA256, gotSHA); err != nil {
		return art, err
	}
	// If the plan didn't carry a SHA256, record it now per
	// cooper-design.md §"Discover JSON contract" ("build streams +
	// hashes; orchestrator should treat such artifacts as re-import
	// unconditionally").
	if art.Asset.SHA256 == nil {
		s := "sha256:" + gotSHA
		art.Asset.SHA256 = &s
		src := plan.SHA256SourceHEADRequest
		art.Asset.SHA256Source = &src
	}

	if IsArchive(art.Asset.Name) {
		if err := Extract(assetPath, assetDir, ExtractOpts{}); err != nil {
			return art, fmt.Errorf("extract %s: %w", art.Asset.Name, err)
		}
		if err := os.Remove(assetPath); err != nil {
			return art, fmt.Errorf("remove archive: %w", err)
		}
	}

	if err := WriteAuxFiles(art.BuildPlan.AuxFiles, staging); err != nil {
		return art, err
	}
	if err := WriteNfpmYAML(art.BuildPlan, filepath.Join(staging, "nfpm.yaml")); err != nil {
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
	env := BuildEnv(versionFromArtifact(pkg.Name, art), art.Arch, assetDir, art.BuildPlan.SourceDateEpoch)
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
