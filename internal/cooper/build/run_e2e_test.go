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

	"github.com/ophymx/apt-signpost/pkg/plan"
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
	}

	_ = filepath.Walk
}
