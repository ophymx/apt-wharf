package discover

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/ophymx/apt-signpost/internal/chandler/config"
	"github.com/ophymx/apt-signpost/internal/chandler/keys"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// keyServer returns an httptest.Server that hands out a fresh
// ASCII-armored OpenPGP public key at /<path>. Tests don't share key
// material — every call creates a new entity.
func keyServer(t *testing.T) *httptest.Server {
	t.Helper()
	e, err := openpgp.NewEntity("Test Vendor", "ci", "vendor@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var armored bytes.Buffer
	aw, err := armor.Encode(&armored, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(aw); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	body := armored.Bytes()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
}

// writeConfig drops a chandler.yaml in a temp dir and returns the
// loaded Config. SkipGit avoids the git-touched-file constraint;
// every test using this lives outside the project's git tree.
func writeConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "chandler.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func mustHaveAux(t *testing.T, pkg plan.Package, key string) []byte {
	t.Helper()
	if len(pkg.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(pkg.Artifacts))
	}
	entry, ok := pkg.Artifacts[0].BuildPlan.AuxFiles[key]
	if !ok {
		t.Fatalf("aux_files[%q] missing; have keys=%v", key, auxKeys(pkg.Artifacts[0].BuildPlan.AuxFiles))
	}
	raw, err := base64.StdEncoding.DecodeString(entry.ContentB64)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", key, err)
	}
	return raw
}

func auxKeys(m map[string]plan.AuxFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRun_SimpleMode(t *testing.T) {
	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: vendor-archive-keyring
  version: "1.0"
  description: Test vendor.
  maintainer: ops@example.com
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://apt.vendor.example
    suites: stable
    components: main
    architectures: [amd64, arm64]
    key: vendor
`, srv.URL)
	cfg := writeConfig(t, yaml)

	var stderr bytes.Buffer
	p, err := Run(context.Background(), cfg, Options{
		Tool:    plan.Tool{Name: "chandler", Version: "test", FormatRevision: plan.FormatRevision},
		Client:  &keys.Client{HTTP: srv.Client()},
		Stderr:  &stderr,
		SkipGit: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.Packages) != 1 {
		t.Fatalf("want 1 package, got %d", len(p.Packages))
	}
	pkg := p.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("package not ok: %+v", pkg.Error)
	}
	if pkg.Name != "vendor-archive-keyring" {
		t.Errorf("name: got %q", pkg.Name)
	}
	if pkg.Source.Kind != plan.SourceKindChandler {
		t.Errorf("source.kind: got %q", pkg.Source.Kind)
	}
	if len(pkg.Source.FetchedKeys) != 1 {
		t.Fatalf("fetched_keys: want 1, got %d", len(pkg.Source.FetchedKeys))
	}
	if pkg.Source.FetchedKeys[0].Fingerprint == "" {
		t.Error("fetched_keys[0].fingerprint should be set")
	}
	if pkg.Artifacts[0].Arch != "all" {
		t.Errorf("arch: want all, got %q", pkg.Artifacts[0].Arch)
	}
	if pkg.Artifacts[0].Deb.Filename != "vendor-archive-keyring_1.0_all.deb" {
		t.Errorf("filename: got %q", pkg.Artifacts[0].Deb.Filename)
	}
	if !strings.HasPrefix(pkg.Artifacts[0].Deb.BuildInputsHash, "sha256:") {
		t.Errorf("build_inputs_hash format: %q", pkg.Artifacts[0].Deb.BuildInputsHash)
	}

	// aux_files: one keyring + one .sources file, no postinst.
	mustHaveAux(t, pkg, "./vendor.gpg")
	src := mustHaveAux(t, pkg, "./vendor.sources")
	body := string(src)
	for _, line := range []string{
		"Enabled: yes",
		"Types: deb",
		"URIs: https://apt.vendor.example",
		"Suites: stable",
		"Components: main",
		"Architectures: amd64 arm64",
		"Signed-By: /usr/share/keyrings/vendor.gpg",
	} {
		if !strings.Contains(body, line) {
			t.Errorf(".sources file missing line %q\n--- body ---\n%s", line, body)
		}
	}
	if _, ok := pkg.Artifacts[0].BuildPlan.AuxFiles["./postinst.sh"]; ok {
		t.Error("postinst.sh should be absent when run_apt_update is false")
	}

	// stderr should log fingerprint + UID.
	if !strings.Contains(stderr.String(), "fingerprint=") {
		t.Errorf("stderr should mention fingerprint, got %q", stderr.String())
	}
}

func TestRun_MatrixMode(t *testing.T) {
	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: "vendor-archive-keyring-{{.Codename}}"
  version: "1.0"
  description: Test vendor across codenames.
  maintainer: ops@example.com
targets:
  - { distro: debian, codename: bookworm }
  - { distro: debian, codename: trixie }
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://apt.vendor.example
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
		t.Fatalf("Run: %v", err)
	}
	if len(p.Packages) != 2 {
		t.Fatalf("want 2 packages, got %d", len(p.Packages))
	}
	wantNames := map[string]string{
		"vendor-archive-keyring-bookworm": "bookworm",
		"vendor-archive-keyring-trixie":   "trixie",
	}
	for _, pkg := range p.Packages {
		codename, ok := wantNames[pkg.Name]
		if !ok {
			t.Errorf("unexpected package name %q", pkg.Name)
			continue
		}
		if pkg.Result != plan.ResultOK {
			t.Errorf("%s not ok: %+v", pkg.Name, pkg.Error)
			continue
		}
		if pkg.Source.Target == nil || pkg.Source.Target.Codename != codename {
			t.Errorf("%s: target codename mismatch: %+v", pkg.Name, pkg.Source.Target)
		}
		src := mustHaveAux(t, pkg, "./vendor.sources")
		want := "Suites: " + codename
		if !strings.Contains(string(src), want) {
			t.Errorf("%s: .sources missing %q\nbody:\n%s", pkg.Name, want, string(src))
		}
	}

	// Each matrix .deb's build_inputs_hash should differ (different
	// rendered .sources files → different aux_files → different hash).
	h1 := p.Packages[0].Artifacts[0].Deb.BuildInputsHash
	h2 := p.Packages[1].Artifacts[0].Deb.BuildInputsHash
	if h1 == h2 {
		t.Error("matrix .debs should have distinct build_inputs_hash; bookworm == trixie")
	}
}

func TestRun_RunAptUpdate(t *testing.T) {
	srv := keyServer(t)
	defer srv.Close()

	yaml := fmt.Sprintf(`
package:
  name: vendor-archive-keyring
  version: "1.0"
  description: x
  maintainer: x
  run_apt_update: true
keys:
  vendor:
    url: %s/key.gpg
sources:
  - id: vendor
    uris: https://example.com
    suites: stable
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
		t.Fatalf("Run: %v", err)
	}
	pkg := p.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("not ok: %+v", pkg.Error)
	}
	postinst := mustHaveAux(t, pkg, "./postinst.sh")
	if !strings.Contains(string(postinst), "apt-get update") {
		t.Errorf("postinst.sh should call apt-get update; got:\n%s", postinst)
	}

	// nfpm subtree's scripts.postinstall must reference the staged path.
	var nfpm map[string]any
	if err := json.Unmarshal(pkg.Artifacts[0].BuildPlan.Nfpm, &nfpm); err != nil {
		t.Fatal(err)
	}
	scripts, _ := nfpm["scripts"].(map[string]any)
	if scripts["postinstall"] != "./postinst.sh" {
		t.Errorf("nfpm.scripts.postinstall: got %v", scripts["postinstall"])
	}
}

func TestRun_KeyFetchFailureIsolated(t *testing.T) {
	// Server that returns 503 — fetch should fail with the chandler
	// error.kind.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	yaml := fmt.Sprintf(`
package:
  name: vendor-archive-keyring
  version: "1.0"
  description: x
  maintainer: x
keys:
  vendor:
    url: %s/k
sources:
  - id: vendor
    uris: https://example.com
    suites: stable
    components: main
    key: vendor
`, dead.URL)
	cfg := writeConfig(t, yaml)

	p, err := Run(context.Background(), cfg, Options{
		Tool:    plan.Tool{Name: "chandler", Version: "test", FormatRevision: plan.FormatRevision},
		Client:  &keys.Client{HTTP: dead.Client()},
		Stderr:  &bytes.Buffer{},
		SkipGit: true,
	})
	if err != nil {
		t.Fatalf("Run itself should not fail on per-target fetch errors: %v", err)
	}
	if len(p.Packages) != 1 {
		t.Fatalf("want 1 package, got %d", len(p.Packages))
	}
	if p.Packages[0].Result != plan.ResultError {
		t.Fatalf("expected ResultError, got %q", p.Packages[0].Result)
	}
	if p.Packages[0].Error.Kind != ErrorKindKeyFetchFailed {
		t.Errorf("error.kind: want %q, got %q", ErrorKindKeyFetchFailed, p.Packages[0].Error.Kind)
	}
}

func TestRun_Reproducible(t *testing.T) {
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
    url: %s/k
sources:
  - id: vendor
    uris: https://example.com
    suites: stable
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
	// DiscoveredAt is the only field free to differ; everything
	// hash-relevant must be identical across runs.
	h1 := p1.Packages[0].Artifacts[0].Deb.BuildInputsHash
	h2 := p2.Packages[0].Artifacts[0].Deb.BuildInputsHash
	if h1 != h2 {
		t.Errorf("build_inputs_hash drifted across runs: %q vs %q", h1, h2)
	}
}

func TestRun_MatrixNameCollisionDetected(t *testing.T) {
	srv := keyServer(t)
	defer srv.Close()

	// Templated name references .Distro but matrix varies only on
	// .Codename — resolves to the same name twice. Collision should
	// be flagged on the second target.
	yaml := fmt.Sprintf(`
package:
  name: "vendor-archive-keyring-{{.Distro}}"
  version: "1.0"
  description: x
  maintainer: x
targets:
  - { distro: debian, codename: bookworm }
  - { distro: debian, codename: trixie }
keys:
  vendor:
    url: %s/k
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
	if len(p.Packages) != 2 {
		t.Fatalf("want 2 packages, got %d", len(p.Packages))
	}
	if p.Packages[1].Result != plan.ResultError {
		t.Fatalf("second package should fail with collision, got %+v", p.Packages[1])
	}
	if !strings.Contains(p.Packages[1].Error.Message, "collides") {
		t.Errorf("error message should mention collision, got %q", p.Packages[1].Error.Message)
	}
}
