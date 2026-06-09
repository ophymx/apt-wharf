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

	"github.com/ophymx/apt-wharf/pkg/plan"
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
	hash, err := plan.ComputeBuildInputsHash(plan.FormatRevision, []*string{&sha}, bp)
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
				Assets: []plan.Asset{{
					Name:         "hugo_extended_0.140.0_linux-amd64.tar.gz",
					URL:          srv.URL + "/asset.tar.gz",
					Size:         int64(len(assetBytes)),
					SHA256:       &sha,
					SHA256Source: &srcVal,
				}},
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

// TestRun_HappyPath drives the full pipeline against the real nfpm
// library: download → extract → write nfpm.yaml → Pack to .deb.
// Asserts the annotated artifact carries the expected filename and a
// sha256, and that the work dir is cleaned up by default.
func TestRun_HappyPath(t *testing.T) {
	asset := makeArchive(t, "ELF...hugo binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	work := t.TempDir()
	out := t.TempDir()

	res, err := Run(context.Background(), p, Options{
		OutDir:  out,
		WorkDir: work,
	})
	if err != nil {
		t.Fatal(err)
	}

	art := res.Packages[0].Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("expected ok, got error: %+v", art.Error)
	}
	if art.Deb.Path == nil || !strings.HasSuffix(*art.Deb.Path, "/hugo_0.140.0_amd64.deb") {
		t.Errorf("deb.path: %v", art.Deb.Path)
	}
	if art.Deb.SHA256 == nil || !strings.HasPrefix(*art.Deb.SHA256, "sha256:") {
		t.Errorf("deb.sha256: %v", art.Deb.SHA256)
	}
	if st, err := os.Stat(*art.Deb.Path); err != nil {
		t.Errorf("deb file missing: %v", err)
	} else if st.Size() < 200 {
		t.Errorf("deb suspiciously small: %d bytes", st.Size())
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
		OutDir:  t.TempDir(),
		WorkDir: t.TempDir(),
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

// TestRun_NfpmFails_BecomesArtifactError forces a Pack-time failure by
// pointing a contents entry at a path that no staging step
// materializes. nfpm's PrepareForPackager calls os.Stat on every src
// and errors when the file is missing — Run() should isolate that as
// an artifact-level error, not a top-level Run() error.
func TestRun_NfpmFails_BecomesArtifactError(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))
	// Replace the working contents with one that references a file
	// neither the asset archive nor aux_files ever produces.
	p.Packages[0].Artifacts[0].BuildPlan.Nfpm = json.RawMessage(`{
		"name": "hugo",
		"version": "0.140.0",
		"arch": "amd64",
		"platform": "linux",
		"maintainer": "Ophymx <ops@ophymx.com>",
		"description": "Static site generator",
		"contents": [
			{"src": "${ASSETS}/does-not-exist", "dst": "/usr/bin/hugo"}
		]
	}`)
	// Rehash so the build-time hash preamble accepts the plan.
	sha := *p.Packages[0].Artifacts[0].Assets[0].SHA256
	bp := p.Packages[0].Artifacts[0].BuildPlan
	newHash, err := plan.ComputeBuildInputsHash(plan.FormatRevision, []*string{&sha}, bp)
	if err != nil {
		t.Fatal(err)
	}
	p.Packages[0].Artifacts[0].Deb.BuildInputsHash = newHash

	res, err := Run(context.Background(), p, Options{
		OutDir:  t.TempDir(),
		WorkDir: t.TempDir(),
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
	out := t.TempDir()

	res, err := Run(context.Background(), p, Options{
		OutDir:    out,
		WorkDir:   work,
		StageOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Packages[0].Artifacts[0].Deb.Path != nil {
		t.Errorf("deb.path should remain nil in stage-only")
	}
	// No .deb should appear under OutDir.
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("out-dir should be empty in stage-only: %v", entries)
	}

	// Work dir preserved with the staged tree.
	workEntries, _ := os.ReadDir(work)
	if len(workEntries) != 1 {
		t.Fatalf("work-dir should keep run dir: got %d entries", len(workEntries))
	}
	stagingNfpm := filepath.Join(work, workEntries[0].Name(), "hugo", "amd64", "nfpm.yaml")
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
	// Deep-copy Assets so we don't mutate armArt's slice through the shared backing array.
	failArt.Assets = append([]plan.Asset(nil), armArt.Assets...)
	failArt.Assets[0].URL = failSrv.URL + "/missing"
	// Recompute hash for failArt because Arch isn't in the hash
	// indirectly (it's in BuildPlan.Nfpm — same in this fixture).
	failArt.Deb.BuildInputsHash = armArt.Deb.BuildInputsHash
	p.Packages[0].Artifacts = []plan.Artifact{failArt, armArt}

	res, err := Run(context.Background(), p, Options{
		OutDir:  t.TempDir(),
		WorkDir: t.TempDir(),
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

// TestRun_Revision_AppendsToFilenameAndStagedYAML asserts --revision N
// rewrites both the .deb filename (annotated JSON) and the staged
// nfpm.yaml — `version:` stays bare, `release: "N"` carries the
// debian-revision. build_inputs_hash is unchanged. The control-field
// roundtrip through nfpm itself is covered by
// TestRun_RealNfpm_Revision in run_e2e_test.go.
func TestRun_Revision_AppendsToFilenameAndStagedYAML(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))
	originalHash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash

	work := t.TempDir()
	res, err := Run(context.Background(), p, Options{
		OutDir:   t.TempDir(),
		WorkDir:  work,
		Revision: 2,
		KeepWork: true,
	})
	if err != nil {
		t.Fatal(err)
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

	out := t.TempDir()
	_, err := Run(context.Background(), p, Options{
		OutDir:   out,
		WorkDir:  t.TempDir(),
		Revision: 1,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--revision incompatible") {
		t.Errorf("error message: %v", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("out-dir should stay empty when --revision rejects upfront: %v", entries)
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

	out := t.TempDir()
	_, err := Run(context.Background(), p, Options{
		OutDir:  out,
		WorkDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "annotated output") {
		t.Errorf("expected annotated-plan error, got %v", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("out-dir should stay empty when annotated-plan rejection fires: %v", entries)
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

// TestRun_AssetOptional asserts cooper build skips the download +
// extract phase when Asset.URL is empty (the local-source case driven
// by staves and other producers that bake every byte into aux_files).
// The aux_files materialization + library Pack call is identical to
// the assets path.
func TestRun_AssetOptional(t *testing.T) {
	// Hand-rolled plan: no asset URL, aux_files carries the only
	// payload (one file destined for /usr/share/demo/hello.txt).
	bp := plan.BuildPlan{
		SourceDateEpoch: 1715240520,
		Nfpm: json.RawMessage(`{
			"name": "demo",
			"version": "1.0.0",
			"arch": "all",
			"platform": "linux",
			"maintainer": "Demo <demo@example.invalid>",
			"description": "asset-optional fixture",
			"contents": [
				{"src": "./hello.txt", "dst": "/usr/share/demo/hello.txt"}
			]
		}`),
		AuxFiles: map[string]plan.AuxFile{
			"./hello.txt": {ContentB64: "aGVsbG8K"}, // "hello\n"
		},
	}
	hash, err := plan.ComputeBuildInputsHash(plan.FormatRevision, nil, bp)
	if err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          plan.Tool{Name: "staves", Version: "test", FormatRevision: plan.FormatRevision},
		Packages: []plan.Package{{
			Name:   "demo",
			Result: plan.ResultOK,
			Source: &plan.Source{Kind: "local"},
			Artifacts: []plan.Artifact{{
				Arch:      "all",
				Assets:    nil, // staves case: no upstream
				Deb:       plan.Deb{Filename: "demo_1.0.0_all.deb", BuildInputsHash: hash},
				BuildPlan: bp,
			}},
		}},
	}

	work := t.TempDir()
	out := t.TempDir()

	res, err := Run(context.Background(), p, Options{
		OutDir:  out,
		WorkDir: work,
	})
	if err != nil {
		t.Fatal(err)
	}
	art := res.Packages[0].Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("expected ok, got error: %+v", art.Error)
	}
	if art.Deb.Path == nil || !strings.HasSuffix(*art.Deb.Path, "/demo_1.0.0_all.deb") {
		t.Errorf("deb.path: %v", art.Deb.Path)
	}
	if _, err := os.Stat(*art.Deb.Path); err != nil {
		t.Errorf("deb file missing: %v", err)
	}
}

// TestRun_RelativeOutDir_ResolvesAbs guards against the
// "open dist/foo.deb: no such file or directory" regression: when
// --out-dir is relative (the default is "./dist"), Run() must
// absolutize it before passing the deb path down to Pack — Pack runs
// nfpm in-process, and a relative outputPath would resolve against
// whatever cwd the caller happens to have, not the staging tree.
func TestRun_RelativeOutDir_ResolvesAbs(t *testing.T) {
	asset := makeArchive(t, "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()
	p := fixturePlan(t, srv, asset, sha256Of(t, asset))

	callerCwd := t.TempDir()
	t.Chdir(callerCwd)

	res, err := Run(context.Background(), p, Options{
		OutDir:  "./relout",
		WorkDir: "./relwork",
	})
	if err != nil {
		t.Fatal(err)
	}
	art := res.Packages[0].Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("expected ok, got error: %+v", art.Error)
	}
	if art.Deb.Path == nil || !filepath.IsAbs(*art.Deb.Path) {
		t.Errorf("annotated deb.path should be absolute: %v", art.Deb.Path)
	}
	// The deb should be where the annotated path claims, regardless of
	// the relative arg the caller passed in.
	if _, err := os.Stat(*art.Deb.Path); err != nil {
		t.Errorf("deb file missing at annotated path: %v", err)
	}
}
