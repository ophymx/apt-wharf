package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/pkg/plan"
)

// TestRun_RealNfpm exercises the full build pipeline against the real
// `nfpm` binary. Skipped automatically when nfpm isn't on PATH so the
// test suite stays portable.
func TestRun_RealNfpm(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}

	asset := makeArchive(t, "fake hugo binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	p := fixturePlan(t, srv, asset, sha256Of(t, asset))
	out := t.TempDir()
	work := t.TempDir()

	res, err := Run(context.Background(), p, Options{
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
		t.Fatal("deb.path nil")
	}
	st, err := os.Stat(*art.Deb.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < 200 {
		t.Errorf(".deb suspiciously small: %d bytes", st.Size())
	}

	// Confirm the .deb is a valid ar archive (Debian magic bytes).
	body, err := os.ReadFile(*art.Deb.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body[:8]), "!<arch>") {
		t.Errorf("not an ar archive: first 8 bytes %q", body[:8])
	}

	// Reproducibility: run a second build on the same plan and compare
	// .deb SHA256s. Equality is the cooper-design.md guarantee.
	out2 := t.TempDir()
	work2 := t.TempDir()
	res2, err := Run(context.Background(), p, Options{
		OutDir:  out2,
		WorkDir: work2,
	})
	if err != nil {
		t.Fatal(err)
	}
	art2 := res2.Packages[0].Artifacts[0]
	if art2.Deb.SHA256 == nil || *art.Deb.SHA256 != *art2.Deb.SHA256 {
		t.Errorf("non-reproducible build: a=%v b=%v", art.Deb.SHA256, art2.Deb.SHA256)
	}

	// dpkg-deb -I sanity check, if available.
	if _, err := exec.LookPath("dpkg-deb"); err == nil {
		out, err := exec.Command("dpkg-deb", "-I", *art.Deb.Path).CombinedOutput()
		if err != nil {
			t.Errorf("dpkg-deb -I failed: %v\n%s", err, out)
		}
		// Verify the control field claims the right Package name.
		if !strings.Contains(string(out), "Package: hugo") {
			t.Errorf("control field missing Package: hugo:\n%s", out)
		}
		// Verify the X-Cooper-Build-Inputs-Hash control field is
		// present and matches the plan's build_inputs_hash.
		field, err := exec.Command("dpkg-deb", "-f", *art.Deb.Path, "X-Cooper-Build-Inputs-Hash").Output()
		if err != nil {
			t.Errorf("dpkg-deb -f X-Cooper-Build-Inputs-Hash failed: %v", err)
		}
		got := strings.TrimSpace(string(field))
		want := art.Deb.BuildInputsHash
		if got != want {
			t.Errorf("X-Cooper-Build-Inputs-Hash mismatch:\n  in .deb:   %q\n  in plan:   %q", got, want)
		}
	}

	_ = filepath.Walk
}

// TestRun_RealNfpm_Revision builds the same fixture twice — once bare,
// once with --revision 1 — and asserts:
//
//  1. The revised .deb's filename is hugo_0.140.0-1_amd64.deb.
//  2. dpkg-deb reports Version: 0.140.0-1 in the revised .deb.
//  3. The X-Cooper-Build-Inputs-Hash control field is *identical* in
//     both .debs — the load-bearing property of the revision-excluded
//     hash design.
//
// Skipped automatically when nfpm or dpkg-deb is unavailable.
func TestRun_RealNfpm_Revision(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not on PATH; skipping field-level e2e")
	}

	asset := makeArchive(t, "fake hugo binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	bareOut, bareWork := t.TempDir(), t.TempDir()
	revOut, revWork := t.TempDir(), t.TempDir()

	bareRes, err := Run(context.Background(), fixturePlan(t, srv, asset, sha256Of(t, asset)), Options{
		OutDir: bareOut, WorkDir: bareWork,
	})
	if err != nil {
		t.Fatal(err)
	}
	revRes, err := Run(context.Background(), fixturePlan(t, srv, asset, sha256Of(t, asset)), Options{
		OutDir: revOut, WorkDir: revWork, Revision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	bareArt := bareRes.Packages[0].Artifacts[0]
	revArt := revRes.Packages[0].Artifacts[0]

	if revArt.Deb.Filename != "hugo_0.140.0-1_amd64.deb" {
		t.Errorf("revised filename: %q, want hugo_0.140.0-1_amd64.deb", revArt.Deb.Filename)
	}
	// dpkg-deb reports the in-package Debian Version.
	out, err := exec.Command("dpkg-deb", "-f", *revArt.Deb.Path, "Version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "0.140.0-1" {
		t.Errorf("dpkg-deb Version: %q, want 0.140.0-1", got)
	}

	// Load-bearing: the X-Cooper-Build-Inputs-Hash field is the SAME
	// across both builds, because the hash is computed from the
	// discover-time plan only and the revision is an on-disk-only
	// adjustment.
	bareField := dpkgDebField(t, *bareArt.Deb.Path, "X-Cooper-Build-Inputs-Hash")
	revField := dpkgDebField(t, *revArt.Deb.Path, "X-Cooper-Build-Inputs-Hash")
	if bareField != revField {
		t.Errorf("X-Cooper-Build-Inputs-Hash drifted under --revision:\n  bare: %q\n  rev:  %q", bareField, revField)
	}
	if bareField != bareArt.Deb.BuildInputsHash {
		t.Errorf("bare field %q != plan hash %q", bareField, bareArt.Deb.BuildInputsHash)
	}
}

func dpkgDebField(t *testing.T, debPath, field string) string {
	t.Helper()
	out, err := exec.Command("dpkg-deb", "-f", debPath, field).Output()
	if err != nil {
		t.Fatalf("dpkg-deb -f %s %s: %v", field, debPath, err)
	}
	return strings.TrimSpace(string(out))
}
