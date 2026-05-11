package discover

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/internal/staves/config"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

const sampleNfpm = `name: demo
version: 1.0.0
arch: all
maintainer: "Demo <demo@example.invalid>"
description: staves discover fixture
contents:
  - src: ./hello.txt
    dst: /usr/share/demo/hello.txt
`

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupRepo builds a self-contained git working tree with a staves
// recipe layout and a single committed package, returning the path to
// staves.yaml. Git is required (the discover pipeline needs commit
// provenance); tests calling this should t.Skip on absence.
func setupRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "staves.yaml"), "packages:\n  - ./packages/demo\n")
	writeFile(t, filepath.Join(dir, "packages/demo/nfpm.yaml"), sampleNfpm)
	writeFile(t, filepath.Join(dir, "packages/demo/hello.txt"), "hello from staves\n")

	for _, cmd := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Staves Test"},
		{"add", "."},
		{"-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture"},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, cmd...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", cmd, err, out)
		}
	}
	return filepath.Join(dir, "staves.yaml")
}

func TestRun_HappyPath(t *testing.T) {
	stavesYaml := setupRepo(t)
	top, err := config.LoadTop(stavesYaml)
	if err != nil {
		t.Fatal(err)
	}

	p, err := Run(context.Background(), top, Options{
		Tool: plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		Now: func() time.Time {
			tm, _ := time.Parse(time.RFC3339, "2026-05-11T00:00:00Z")
			return tm
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if p.SchemaVersion != plan.SchemaVersion {
		t.Errorf("schema_version: %d", p.SchemaVersion)
	}
	if p.DiscoveredAt != "2026-05-11T00:00:00Z" {
		t.Errorf("discovered_at: %s", p.DiscoveredAt)
	}
	if len(p.Packages) != 1 {
		t.Fatalf("packages: %d", len(p.Packages))
	}
	pkg := p.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("result=%s err=%+v", pkg.Result, pkg.Error)
	}
	if pkg.Name != "demo" {
		t.Errorf("name: %s", pkg.Name)
	}
	if pkg.Source == nil || pkg.Source.Kind != plan.SourceKindLocal {
		t.Errorf("source: %+v", pkg.Source)
	}
	if pkg.Source.GitCommit == "" || pkg.Source.GitDate == "" {
		t.Errorf("git provenance missing: %+v", pkg.Source)
	}

	if len(pkg.Artifacts) != 1 {
		t.Fatalf("artifacts: %d", len(pkg.Artifacts))
	}
	a := pkg.Artifacts[0]
	if a.Arch != "all" {
		t.Errorf("arch: %s", a.Arch)
	}
	if a.Asset.URL != "" || a.Asset.Name != "" {
		t.Errorf("asset should be empty for local source: %+v", a.Asset)
	}
	if a.Deb.Filename != "demo_1.0.0_all.deb" {
		t.Errorf("deb.filename: %s", a.Deb.Filename)
	}

	// build_inputs_hash recomputes — same plan, same hash.
	ok, recomputed, err := plan.VerifyBuildInputsHash(plan.Tool{FormatRevision: plan.FormatRevision}, a)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("build_inputs_hash mismatch: stored=%s recomputed=%s", a.Deb.BuildInputsHash, recomputed)
	}

	// SourceDateEpoch matches the git commit time (parsed back from the source).
	when, err := time.Parse(time.RFC3339, pkg.Source.GitDate)
	if err != nil {
		t.Fatal(err)
	}
	if a.BuildPlan.SourceDateEpoch != when.Unix() {
		t.Errorf("source_date_epoch %d != git_date %d", a.BuildPlan.SourceDateEpoch, when.Unix())
	}

	// aux_files: hello.txt inlined verbatim.
	aux, ok := a.BuildPlan.AuxFiles["./hello.txt"]
	if !ok {
		t.Fatalf("missing aux entry: %v", a.BuildPlan.AuxFiles)
	}
	body, _ := base64.StdEncoding.DecodeString(aux.ContentB64)
	if string(body) != "hello from staves\n" {
		t.Errorf("aux body: %q", body)
	}
}

func TestRun_DeterministicAcrossRuns(t *testing.T) {
	stavesYaml := setupRepo(t)
	top, err := config.LoadTop(stavesYaml)
	if err != nil {
		t.Fatal(err)
	}
	tool := plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision}

	a, err := Run(context.Background(), top, Options{Tool: tool})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), top, Options{Tool: tool})
	if err != nil {
		t.Fatal(err)
	}
	if a.Packages[0].Artifacts[0].Deb.BuildInputsHash != b.Packages[0].Artifacts[0].Deb.BuildInputsHash {
		t.Errorf("hash drift across runs: %s vs %s",
			a.Packages[0].Artifacts[0].Deb.BuildInputsHash,
			b.Packages[0].Artifacts[0].Deb.BuildInputsHash)
	}
}

func TestRun_RejectsAssetSubstitution(t *testing.T) {
	stavesYaml := setupRepo(t)
	// Replace the recipe with one that uses ${ASSETS} — must fail
	// since staves has no upstream asset.
	repoRoot := filepath.Dir(stavesYaml)
	badNfpm := strings.Replace(sampleNfpm, "./hello.txt", "${ASSETS}/hello.txt", 1)
	writeFile(t, filepath.Join(repoRoot, "packages/demo/nfpm.yaml"), badNfpm)

	top, err := config.LoadTop(stavesYaml)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Run(context.Background(), top, Options{
		Tool: plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := p.Packages[0]
	if pkg.Result != plan.ResultError {
		t.Fatalf("expected error result, got %+v", pkg)
	}
	if pkg.Error.Kind != plan.ErrorKindAuxResolutionFailed {
		t.Errorf("error.kind: %s", pkg.Error.Kind)
	}
	if !strings.Contains(pkg.Error.Message, "local files only") {
		t.Errorf("error message: %s", pkg.Error.Message)
	}
}

func TestRun_RejectsNotInGitRepo(t *testing.T) {
	dir := t.TempDir() // no git init
	writeFile(t, filepath.Join(dir, "staves.yaml"), "packages:\n  - ./packages/demo\n")
	writeFile(t, filepath.Join(dir, "packages/demo/nfpm.yaml"), sampleNfpm)
	writeFile(t, filepath.Join(dir, "packages/demo/hello.txt"), "hello\n")

	top, err := config.LoadTop(filepath.Join(dir, "staves.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Run(context.Background(), top, Options{
		Tool: plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := p.Packages[0]
	if pkg.Result != plan.ResultError {
		t.Fatalf("expected error result, got %+v", pkg)
	}
	if !strings.Contains(pkg.Error.Message, "git working tree") {
		t.Errorf("error message: %s", pkg.Error.Message)
	}
}

// TestRun_GoldenJSONShape sanity-checks the wire format mentions the
// local-source provenance fields.
func TestRun_GoldenJSONShape(t *testing.T) {
	stavesYaml := setupRepo(t)
	top, err := config.LoadTop(stavesYaml)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Run(context.Background(), top, Options{
		Tool: plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"kind": "local"`,
		`"git_commit":`,
		`"git_date":`,
		`"build_inputs_hash":`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in JSON output", want)
		}
	}
}
