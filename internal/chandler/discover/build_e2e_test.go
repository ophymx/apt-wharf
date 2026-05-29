package discover

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/chandler/keys"
	cooperbuild "github.com/ophymx/apt-wharf/internal/cooper/build"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// TestE2E_BuildsDeb runs the full chandler → cooper-build pipeline
// end-to-end against the real `nfpm` binary. Skipped automatically
// when nfpm isn't on PATH so the test suite stays portable.
//
// What this verifies that no other test can:
//   - The nfpm subtree chandler emits is a shape cooper-build can write
//     out as a valid nfpm.yaml and nfpm itself can read.
//   - The aux_files map round-trips through cooper-build's staging
//     step and ends up as the right files on the right paths in the
//     resulting .deb.
//   - The resulting .deb is reproducible: identical input plan →
//     identical .deb SHA256.
func TestE2E_BuildsDeb(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}

	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: vendor-archive-keyring
  version: "1.0"
  description: APT sources and signing key for vendor.
  maintainer: "test <test@example.com>"
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://apt.vendor.example
    suites: bookworm
    components: main
    architectures: [amd64, arm64]
    key: vendor
`, srv.URL)
	cfg := writeConfig(t, yaml)

	p1, err := Run(context.Background(), cfg, Options{
		Tool:    plan.Tool{Name: "chandler", Version: "test", FormatRevision: plan.FormatRevision},
		Client:  &keys.Client{HTTP: srv.Client()},
		Stderr:  &bytes.Buffer{},
		SkipGit: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	work := t.TempDir()
	res, err := cooperbuild.Run(context.Background(), p1, cooperbuild.Options{
		OutDir:  out,
		WorkDir: work,
	})
	if err != nil {
		t.Fatalf("cooper build: %v", err)
	}
	if len(res.Packages) != 1 || len(res.Packages[0].Artifacts) != 1 {
		t.Fatalf("expected 1 package × 1 artifact; got %d × varies", len(res.Packages))
	}
	art := res.Packages[0].Artifacts[0]
	if art.Result == plan.ResultError {
		t.Fatalf("build failed: %s", art.Error.Message)
	}
	if art.Deb.Path == nil {
		t.Fatal("deb.path nil — cooper build didn't materialize a .deb")
	}
	if !strings.HasSuffix(*art.Deb.Path, "vendor-archive-keyring_1.0_all.deb") {
		t.Errorf("unexpected .deb filename: %q", *art.Deb.Path)
	}

	// Contents listing — assert the right files made it in.
	contents := dpkgDebList(t, *art.Deb.Path)
	for _, want := range []string{
		"./usr/share/keyrings/vendor.gpg",
		"./etc/apt/sources.list.d/vendor.sources",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("contents missing %q\n--- listing ---\n%s", want, contents)
		}
	}

	// .sources content — chandler's deb822 rendering should be intact
	// after cooper build's staging step.
	sources := dpkgDebExtractFile(t, *art.Deb.Path, "./etc/apt/sources.list.d/vendor.sources")
	for _, want := range []string{
		"Enabled: yes",
		"Types: deb",
		"URIs: https://apt.vendor.example",
		"Suites: bookworm",
		"Components: main",
		"Architectures: amd64 arm64",
		"Signed-By: /usr/share/keyrings/vendor.gpg",
	} {
		if !strings.Contains(sources, want) {
			t.Errorf(".sources missing line %q\n--- body ---\n%s", want, sources)
		}
	}

	// Control fields — Architecture, Section, Priority, Depends.
	for field, want := range map[string]string{
		"Architecture": "all",
		"Section":      "misc",
		"Priority":     "optional",
		"Package":      "vendor-archive-keyring",
	} {
		got := dpkgDebField(t, *art.Deb.Path, field)
		if got != want {
			t.Errorf("control %s: got %q, want %q", field, got, want)
		}
	}
	depends := dpkgDebField(t, *art.Deb.Path, "Depends")
	if !strings.Contains(depends, "apt (>= 1.5)") {
		t.Errorf("Depends should include apt (>= 1.5); got %q", depends)
	}

	// X-Cooper-Build-Inputs-Hash matches the plan.
	embedded := dpkgDebField(t, *art.Deb.Path, "X-Cooper-Build-Inputs-Hash")
	if embedded != art.Deb.BuildInputsHash {
		t.Errorf("X-Cooper-Build-Inputs-Hash mismatch:\n  .deb:  %q\n  plan:  %q", embedded, art.Deb.BuildInputsHash)
	}
}

// TestE2E_Reproducible: chandler discover twice → cooper build twice →
// identical .deb SHA256s. The cooper-design.md reproducibility
// guarantee inherited by chandler.
func TestE2E_Reproducible(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}

	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: vendor-archive-keyring
  version: "1.0"
  description: x
  maintainer: x
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://example.com
    suites: bookworm
    components: main
    key: vendor
`, srv.URL)
	cfg := writeConfig(t, yaml)

	opts := Options{
		Tool:    plan.Tool{Name: "chandler", Version: "test", FormatRevision: plan.FormatRevision},
		Client:  &keys.Client{HTTP: srv.Client()},
		Stderr:  &bytes.Buffer{},
		SkipGit: true,
	}
	p1, err := Run(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Run(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	build := func(p *plan.Plan) string {
		t.Helper()
		out := t.TempDir()
		work := t.TempDir()
		res, err := cooperbuild.Run(context.Background(), p, cooperbuild.Options{
			OutDir: out, WorkDir: work,
		})
		if err != nil {
			t.Fatal(err)
		}
		art := res.Packages[0].Artifacts[0]
		if art.Result == plan.ResultError {
			t.Fatalf("build: %s", art.Error.Message)
		}
		if art.Deb.SHA256 == nil {
			t.Fatal("deb.sha256 nil")
		}
		return *art.Deb.SHA256
	}
	h1 := build(p1)
	h2 := build(p2)
	if h1 != h2 {
		t.Errorf(".deb not byte-reproducible across two discover→build passes:\n  a: %s\n  b: %s", h1, h2)
	}
}

// TestE2E_Matrix builds a 2-target matrix and asserts the two .debs
// have distinct names + distinct hashes.
func TestE2E_Matrix(t *testing.T) {
	if _, err := exec.LookPath("nfpm"); err != nil {
		t.Skip("nfpm not on PATH; skipping live e2e")
	}

	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: "vendor-archive-keyring-{{.Codename}}"
  version: "1.0"
  description: x
  maintainer: x
targets:
  - { distro: debian, codename: bookworm }
  - { distro: debian, codename: trixie }
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://example.com
    suites: "{{.Codename}}"
    components: main
    key: vendor
`, srv.URL)
	cfg := writeConfig(t, yaml)

	p, err := Run(context.Background(), cfg, Options{
		Tool:    plan.Tool{Name: "chandler", Version: "test", FormatRevision: plan.FormatRevision},
		Client:  &keys.Client{HTTP: srv.Client()},
		Stderr:  &bytes.Buffer{},
		SkipGit: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	work := t.TempDir()
	res, err := cooperbuild.Run(context.Background(), p, cooperbuild.Options{
		OutDir: out, WorkDir: work,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Packages) != 2 {
		t.Fatalf("want 2 packages, got %d", len(res.Packages))
	}

	hashes := make(map[string]bool)
	codenames := make(map[string]bool)
	for _, pkg := range res.Packages {
		if pkg.Result == plan.ResultError {
			t.Fatalf("%s: %s", pkg.Name, pkg.Error.Message)
		}
		art := pkg.Artifacts[0]
		if art.Deb.SHA256 == nil {
			t.Fatalf("%s: sha256 nil", pkg.Name)
		}
		hashes[*art.Deb.SHA256] = true

		// Confirm each .deb's .sources contains the matching codename.
		body := dpkgDebExtractFile(t, *art.Deb.Path, "./etc/apt/sources.list.d/vendor.sources")
		for _, codename := range []string{"bookworm", "trixie"} {
			if strings.Contains(body, "Suites: "+codename) {
				codenames[codename] = true
			}
		}
	}
	if len(hashes) != 2 {
		t.Errorf("expected 2 distinct .deb SHA256s, got %d (some matrix .debs are identical)", len(hashes))
	}
	for _, c := range []string{"bookworm", "trixie"} {
		if !codenames[c] {
			t.Errorf("no .deb has the %s suite", c)
		}
	}
}

func dpkgDebList(t *testing.T, debPath string) string {
	t.Helper()
	out, err := exec.Command("dpkg-deb", "-c", debPath).Output()
	if err != nil {
		t.Fatalf("dpkg-deb -c %s: %v", debPath, err)
	}
	return string(out)
}

func dpkgDebExtractFile(t *testing.T, debPath, member string) string {
	t.Helper()
	// dpkg-deb --fsys-tarfile streams the inner data.tar; pipe into tar
	// to extract one member to stdout.
	cmd := exec.Command("sh", "-c",
		fmt.Sprintf("dpkg-deb --fsys-tarfile %q | tar -xO %q", debPath, member))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("extract %s from %s: %v", member, debPath, err)
	}
	return string(out)
}

func dpkgDebField(t *testing.T, debPath, field string) string {
	t.Helper()
	out, err := exec.Command("dpkg-deb", "-f", debPath, field).Output()
	if err != nil {
		t.Fatalf("dpkg-deb -f %s %s: %v", field, debPath, err)
	}
	return strings.TrimSpace(string(out))
}
