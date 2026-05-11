package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ophymx/apt-signpost/pkg/plan"
)

// makeArchive returns the raw bytes of a .tar.gz containing one
// executable file `hugo/bin/hugo` with the given body.
func makeArchive(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "hugo/bin/hugo",
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fixturePlan builds a self-consistent one-package one-arch plan
// pointing at srv for the asset URL. The build_inputs_hash is computed
// fresh so verification passes.
func fixturePlan(t *testing.T, srv *httptest.Server, assetBytes []byte, sha256Hex string) *plan.Plan {
	t.Helper()
	tool := plan.Tool{Name: "cooper", Version: "test", FormatRevision: plan.FormatRevision}

	bp := plan.BuildPlan{
		SourceDateEpoch: 1715240520,
		Nfpm: json.RawMessage(`{
			"name": "hugo",
			"version": "0.140.0",
			"arch": "amd64",
			"platform": "linux",
			"maintainer": "Ophymx <ops@ophymx.com>",
			"description": "Static site generator",
			"contents": [
				{"src": "${ASSETS}/hugo/bin/hugo", "dst": "/usr/bin/hugo"}
			]
		}`),
		AuxFiles: map[string]plan.AuxFile{},
	}

	sha := "sha256:" + sha256Hex
	hash, err := plan.ComputeBuildInputsHash(plan.FormatRevision, &sha, bp)
	if err != nil {
		t.Fatal(err)
	}

	srcVal := plan.SHA256SourceGitHubAPI
	return &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          tool,
		DiscoveredAt:  "2026-05-10T12:34:56Z",
		Packages: []plan.Package{{
			Name:   "hugo",
			Result: plan.ResultOK,
			Source: &plan.Source{
				Kind:               plan.SourceKindGitHubRelease,
				Repo:               "gohugoio/hugo",
				ReleaseID:          1,
				ReleaseTag:         "v0.140.0",
				ReleasePublishedAt: "2026-05-09T08:12:00Z",
			},
			Artifacts: []plan.Artifact{{
				Arch: "amd64",
				Asset: plan.Asset{
					Name:         "hugo_extended_0.140.0_linux-amd64.tar.gz",
					URL:          srv.URL + "/asset.tar.gz",
					Size:         int64(len(assetBytes)),
					SHA256:       &sha,
					SHA256Source: &srcVal,
				},
				Deb: plan.Deb{
					Filename:        "hugo_0.140.0_amd64.deb",
					BuildInputsHash: hash,
				},
				BuildPlan: bp,
			}},
		}},
	}
}

func sha256Of(t *testing.T, body []byte) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	hex, err := hashFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return hex
}

func TestRun_HappyPath(t *testing.T) {
	asset := makeArchive(t, "ELF...hugo binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	work := t.TempDir()
	out := t.TempDir()

	stubCalled := 0
	var stubEnv []string
	var stubDir string
	var stubOutPath string

	stubExec := func(_ context.Context, dir, outputPath string, env []string) error {
		stubCalled++
		stubEnv = append([]string(nil), env...)
		stubDir = dir
		stubOutPath = outputPath
		// Fake nfpm: write a token .deb so the orchestrator can hash it.
		return os.WriteFile(outputPath, []byte("fake-deb"), 0o644)
	}

	res, err := Run(context.Background(), p, Options{
		OutDir:   out,
		WorkDir:  work,
		NfpmExec: stubExec,
	})
	if err != nil {
		t.Fatal(err)
	}

	if stubCalled != 1 {
		t.Errorf("nfpm should run once, got %d", stubCalled)
	}
	if got := envMap(stubEnv); got["VERSION"] != "0.140.0" || got["ARCH"] != "amd64" {
		t.Errorf("env: %v", got)
	}
	if got := envMap(stubEnv); !strings.HasSuffix(got["ASSETS"], "/hugo/amd64/asset") {
		t.Errorf("ASSETS env not at staging asset dir: %s", got["ASSETS"])
	}
	if !strings.HasSuffix(stubDir, "/hugo/amd64") {
		t.Errorf("nfpm cwd not at staging: %s", stubDir)
	}
	if !strings.HasSuffix(stubOutPath, "/hugo_0.140.0_amd64.deb") {
		t.Errorf("nfpm -t arg: %s", stubOutPath)
	}

	pkg := res.Packages[0]
	art := pkg.Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("expected ok, got error: %+v", art.Error)
	}
	if art.Deb.Path == nil || !strings.HasSuffix(*art.Deb.Path, "/hugo_0.140.0_amd64.deb") {
		t.Errorf("deb.path: %v", art.Deb.Path)
	}
	if art.Deb.SHA256 == nil || !strings.HasPrefix(*art.Deb.SHA256, "sha256:") {
		t.Errorf("deb.sha256: %v", art.Deb.SHA256)
	}

	// Work dir cleaned up by default.
	entries, _ := os.ReadDir(work)
	if len(entries) != 0 {
		t.Errorf("work dir should be cleaned: %v", entries)
	}
}

func TestRun_AssetSHAMismatch_BecomesArtifactError(t *testing.T) {
	asset := makeArchive(t, "real")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	// Use a wrong sha256 in the plan; build will detect mismatch.
	p := fixturePlan(t, srv, asset, "deadbeef00000000000000000000000000000000000000000000000000000000")

	res, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  t.TempDir(),
		NfpmExec: func(context.Context, string, string, []string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	art := res.Packages[0].Artifacts[0]
	if art.Result != plan.ResultError {
		t.Fatalf("expected error, got: %+v", art)
	}
	if art.Error.Kind != plan.ErrorKindBuildFailed {
		t.Errorf("error.kind: %s", art.Error.Kind)
	}
	// The fixture's asset.sha256 was fabricated, but build_inputs_hash
	// was recomputed against that fabrication so verify() passes. The
	// failure mode is the post-download SHA check.
	if !strings.Contains(art.Error.Message, "sha256 mismatch") {
		t.Errorf("error message: %s", art.Error.Message)
	}
}

func TestRun_NfpmFails_BecomesArtifactError(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	res, err := Run(context.Background(), p, Options{
		OutDir:  t.TempDir(),
		WorkDir: t.TempDir(),
		NfpmExec: func(context.Context, string, string, []string) error {
			return os.ErrPermission
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	art := res.Packages[0].Artifacts[0]
	if art.Result != plan.ResultError {
		t.Errorf("expected artifact error, got: %+v", art)
	}
	if !AnyArtifactFailed(res) {
		t.Errorf("AnyArtifactFailed should be true")
	}
}

func TestRun_StageOnly_SkipsNfpmAndKeepsWork(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	work := t.TempDir()
	calls := 0
	stub := func(context.Context, string, string, []string) error { calls++; return nil }

	res, err := Run(context.Background(), p, Options{
		OutDir:    t.TempDir(),
		WorkDir:   work,
		StageOnly: true,
		NfpmExec:  stub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("nfpm should not be called in stage-only mode")
	}
	if res.Packages[0].Artifacts[0].Deb.Path != nil {
		t.Errorf("deb.path should remain nil in stage-only")
	}

	// Work dir preserved with the staged tree.
	entries, _ := os.ReadDir(work)
	if len(entries) != 1 {
		t.Fatalf("work-dir should keep run dir: got %d entries", len(entries))
	}
	stagingNfpm := filepath.Join(work, entries[0].Name(), "hugo", "amd64", "nfpm.yaml")
	if _, err := os.Stat(stagingNfpm); err != nil {
		t.Errorf("nfpm.yaml should exist in staging: %v", err)
	}
}

func TestRun_SiblingArtifactNotBlockedByFailure(t *testing.T) {
	// Build a plan with two arches; the amd64 server returns 404 (download fail),
	// the arm64 server returns the asset successfully.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer failSrv.Close()

	asset := makeArchive(t, "real-arm64")
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer okSrv.Close()

	p := fixturePlan(t, okSrv, asset, sha256Of(t, asset))
	// Add a second artifact (amd64-fail) BEFORE arm64.
	armArt := p.Packages[0].Artifacts[0]
	armArt.Arch = "arm64"
	failArt := armArt
	failArt.Arch = "amd64"
	failArt.Asset.URL = failSrv.URL + "/missing"
	// Recompute hash for failArt because Arch isn't in the hash
	// indirectly (it's in BuildPlan.Nfpm — same in this fixture).
	failArt.Deb.BuildInputsHash = armArt.Deb.BuildInputsHash
	p.Packages[0].Artifacts = []plan.Artifact{failArt, armArt}

	res, err := Run(context.Background(), p, Options{
		OutDir:  t.TempDir(),
		WorkDir: t.TempDir(),
		NfpmExec: func(_ context.Context, _, outputPath string, _ []string) error {
			return os.WriteFile(outputPath, []byte("x"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	arts := res.Packages[0].Artifacts
	if arts[0].Result != plan.ResultError {
		t.Errorf("first (amd64) should fail")
	}
	if arts[1].Result == plan.ResultError {
		t.Errorf("second (arm64) should succeed; got: %+v", arts[1].Error)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

// TestRun_Revision_AppendsToFilenameAndVersion asserts --revision N
// rewrites both the .deb filename (annotated JSON) and the nfpm.yaml
// version field at on-disk-write time, while leaving build_inputs_hash
// unchanged.
func TestRun_Revision_AppendsToFilenameAndVersion(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))
	originalHash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash

	work := t.TempDir()
	var stubOutPath string
	var stubEnv []string
	stub := func(_ context.Context, _, outPath string, env []string) error {
		stubOutPath = outPath
		stubEnv = append([]string(nil), env...)
		return os.WriteFile(outPath, []byte("fake"), 0o644)
	}

	res, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  work,
		Revision: 2,
		KeepWork: true,
		NfpmExec: stub,
	})
	if err != nil {
		t.Fatal(err)
	}

	// nfpm exec received the revised filename.
	if !strings.HasSuffix(stubOutPath, "/hugo_0.140.0-2_amd64.deb") {
		t.Errorf("nfpm output path: %s, want suffix /hugo_0.140.0-2_amd64.deb", stubOutPath)
	}
	// VERSION env is the revised value (consumers of ${VERSION} in
	// nfpm.yaml see the same string as the published deb).
	if got := envMap(stubEnv); got["VERSION"] != "0.140.0-2" {
		t.Errorf("VERSION env: %q, want 0.140.0-2", got["VERSION"])
	}
	// Annotated JSON: filename revised, hash unchanged.
	art := res.Packages[0].Artifacts[0]
	if art.Deb.Filename != "hugo_0.140.0-2_amd64.deb" {
		t.Errorf("deb.filename: %q, want hugo_0.140.0-2_amd64.deb", art.Deb.Filename)
	}
	if art.Deb.BuildInputsHash != originalHash {
		t.Errorf("build_inputs_hash changed under --revision: was %s, now %s", originalHash, art.Deb.BuildInputsHash)
	}

	// On-disk nfpm.yaml: version stays bare; release: 2 carries the
	// debian-revision (nfpm appends "-2" at build time).
	entries, _ := os.ReadDir(work)
	if len(entries) != 1 {
		t.Fatalf("expected one run dir, got %d", len(entries))
	}
	yamlPath := filepath.Join(work, entries[0].Name(), "hugo", "amd64", "nfpm.yaml")
	body, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "version: 0.140.0\n") {
		t.Errorf("nfpm.yaml version should stay bare:\n%s", body)
	}
	if !strings.Contains(string(body), "release: \"2\"") {
		t.Errorf("nfpm.yaml missing release: \"2\":\n%s", body)
	}
}

// TestRun_Revision_RejectsVersionContainingHyphen asserts cooper errors
// out fast when --revision is used against a plan whose nfpm.version
// already carries a debian-revision (a recipe-baked one). Validation
// happens before any artifact is built.
func TestRun_Revision_RejectsVersionContainingHyphen(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	// Replace nfpm with a version that contains "-".
	p.Packages[0].Artifacts[0].BuildPlan.Nfpm = json.RawMessage(`{
		"name": "hugo",
		"version": "0.140.0-rc1",
		"arch": "amd64",
		"platform": "linux"
	}`)

	called := 0
	stub := func(context.Context, string, string, []string) error { called++; return nil }
	_, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  t.TempDir(),
		Revision: 1,
		NfpmExec: stub,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--revision incompatible") {
		t.Errorf("error message: %v", err)
	}
	if called != 0 {
		t.Errorf("nfpm should not run when --revision validation fails (got %d calls)", called)
	}
}

// TestRun_Revision_RejectsRecipeBakedRelease asserts the validation
// also catches a recipe that pre-populates nfpm's dedicated `release:`
// field (the other way a recipe can claim the debian-revision slot).
func TestRun_Revision_RejectsRecipeBakedRelease(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	p.Packages[0].Artifacts[0].BuildPlan.Nfpm = json.RawMessage(`{
		"name": "hugo",
		"version": "0.140.0",
		"release": "1",
		"arch": "amd64",
		"platform": "linux"
	}`)

	_, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  t.TempDir(),
		Revision: 1,
		NfpmExec: func(context.Context, string, string, []string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "nfpm.release") {
		t.Errorf("expected release-conflict error, got %v", err)
	}
}

// TestRun_RejectsAnnotatedPlan asserts cooper build refuses a plan
// that already looks like its own annotated output (deb.path populated
// on any artifact). This is the re-fed-plan guard described in
// cooper-design.md §"--revision N mechanics".
func TestRun_RejectsAnnotatedPlan(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))
	// Simulate a prior cooper build run populating deb.path.
	priorPath := "/tmp/dist/hugo_0.140.0_amd64.deb"
	p.Packages[0].Artifacts[0].Deb.Path = &priorPath

	called := 0
	stub := func(context.Context, string, string, []string) error { called++; return nil }
	_, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  t.TempDir(),
		NfpmExec: stub,
	})
	if err == nil || !strings.Contains(err.Error(), "annotated output") {
		t.Errorf("expected annotated-plan error, got %v", err)
	}
	if called != 0 {
		t.Errorf("nfpm should not run when annotated-plan rejection fires (got %d calls)", called)
	}
}

// TestRun_RejectsNegativeRevision asserts the explicit negative-value
// guard fires in Run() (the CLI also rejects, but Run() is callable from
// orchestrators directly).
func TestRun_RejectsNegativeRevision(t *testing.T) {
	p := &plan.Plan{Packages: []plan.Package{}}
	_, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  t.TempDir(),
		Revision: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("expected positive-integer error, got %v", err)
	}
}
