package discover

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cooperbuild "github.com/ophymx/apt-signpost/internal/cooper/build"
	"github.com/ophymx/apt-signpost/internal/staves/config"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// TestE2E_StavesDiscoverThenCooperBuild proves the architectural seam:
// staves discover emits a plan with Asset.URL == "", cooper build
// accepts that plan, skips the download/extract phase, stages the
// aux_files, exec's nfpm, and produces a real .deb whose
// X-Cooper-Build-Inputs-Hash matches the discover-time hash.
//
// Skipped automatically when nfpm or dpkg-deb isn't available (so the
// regular `go test ./...` sweep stays portable). Run with nfpm
// installed for a real exercise of the full pipeline.
func TestE2E_StavesDiscoverThenCooperBuild(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not on PATH; skipping live e2e")
	}

	stavesYaml := setupRepo(t)
	top, err := config.LoadTop(stavesYaml)
	if err != nil {
		t.Fatal(err)
	}

	tool := plan.Tool{Name: "staves", Version: "0.0.0-test", FormatRevision: plan.FormatRevision}
	p, err := Run(context.Background(), top, Options{Tool: tool})
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	work := t.TempDir()
	res, err := cooperbuild.Run(context.Background(), p, cooperbuild.Options{
		OutDir:  out,
		WorkDir: work,
	})
	if err != nil {
		t.Fatal(err)
	}

	art := res.Packages[0].Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("build failed: %s", art.Error.Message)
	}
	if art.Deb.Path == nil {
		t.Fatal("deb.path nil after build")
	}
	if _, err := os.Stat(*art.Deb.Path); err != nil {
		t.Fatalf("deb missing on disk: %v", err)
	}
	if !strings.HasSuffix(*art.Deb.Path, "/demo_1.0.0_all.deb") {
		t.Errorf("deb path: %s", *art.Deb.Path)
	}

	// Embedded X-Cooper-Build-Inputs-Hash matches the discover-time
	// hash — staves participates in drayman's dedup-by-hash query
	// exactly like cooper does.
	field := dpkgDebField(t, *art.Deb.Path, "X-Cooper-Build-Inputs-Hash")
	if field != art.Deb.BuildInputsHash {
		t.Errorf("X-Cooper-Build-Inputs-Hash:\n  in .deb:   %q\n  in plan:   %q", field, art.Deb.BuildInputsHash)
	}

	// The .deb actually contains our file — sanity-check via dpkg-deb -c.
	out2, err := exec.Command("dpkg-deb", "-c", *art.Deb.Path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "./usr/share/demo/hello.txt") {
		t.Errorf("dpkg-deb -c missing expected file:\n%s", out2)
	}

	// Reproducibility: a second build against the same plan yields
	// a byte-identical .deb. This is the cooper-design.md guarantee
	// extended to the staves source.
	out2dir := t.TempDir()
	work2 := t.TempDir()
	res2, err := cooperbuild.Run(context.Background(), p, cooperbuild.Options{
		OutDir:  out2dir,
		WorkDir: work2,
	})
	if err != nil {
		t.Fatal(err)
	}
	art2 := res2.Packages[0].Artifacts[0]
	if art2.Deb.SHA256 == nil || art.Deb.SHA256 == nil || *art.Deb.SHA256 != *art2.Deb.SHA256 {
		t.Errorf("non-reproducible staves+cooper build: a=%v b=%v", art.Deb.SHA256, art2.Deb.SHA256)
	}
}

// dpkgDebField is a local helper duplicated from the cooper-build
// e2e test (they live in different packages and the helper is small).
func dpkgDebField(t *testing.T, debPath, field string) string {
	t.Helper()
	out, err := exec.Command("dpkg-deb", "-f", debPath, field).Output()
	if err != nil {
		t.Fatalf("dpkg-deb -f %s %s: %v", field, debPath, err)
	}
	return strings.TrimSpace(string(out))
}

var _ = filepath.Walk // silence unused-import warning if filepath isn't otherwise touched
