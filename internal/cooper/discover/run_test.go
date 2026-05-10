package discover

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
	signsource "github.com/ophymx/apt-signpost/internal/source"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

const samplePackageBody = `---
source:
  github:
    repo: gohugoio/hugo
    release: latest
version_from: tag_strip_v
arches:
  amd64:
    asset: "hugo_extended_${VERSION}_linux-amd64.tar.gz"
  arm64:
    asset: "hugo_extended_${VERSION}_linux-arm64.tar.gz"
---
name: hugo
version: ${VERSION}
arch: ${ARCH}
maintainer: "Ophymx <ops@ophymx.com>"
description: A fast static site generator
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
  - src: ./hugo.service
    dst: /lib/systemd/system/hugo.service
`

const releaseJSON = `{
  "id": 178213984,
  "tag_name": "v0.140.0",
  "draft": false,
  "prerelease": false,
  "published_at": "2026-05-09T08:12:00Z",
  "assets": [
    {
      "name": "hugo_extended_0.140.0_linux-amd64.tar.gz",
      "size": 19283746,
      "digest": "sha256:abc123",
      "browser_download_url": "https://example.invalid/hugo_amd64.tar.gz"
    },
    {
      "name": "hugo_extended_0.140.0_linux-arm64.tar.gz",
      "size": 18283746,
      "digest": "sha256:def456",
      "browser_download_url": "https://example.invalid/hugo_arm64.tar.gz"
    }
  ]
}`

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setupRepo(t *testing.T) (cooperYaml string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "packages/hugo.yaml"), samplePackageBody)
	writeFile(t, filepath.Join(dir, "packages/hugo.service"), "[Unit]\nDescription=Hugo\n")
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/hugo.yaml\n")
	return filepath.Join(dir, "cooper.yaml")
}

func newTestServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/gohugoio/hugo/releases/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releaseJSON))
	}))
}

func testClient(t *testing.T, srv *httptest.Server) Options {
	t.Helper()
	c := signsource.NewGitHubClient(srv.Client(), nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	return Options{
		Tool:   plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		Client: c,
		Now: func() time.Time {
			t, _ := time.Parse(time.RFC3339, "2026-05-10T12:34:56Z")
			return t
		},
	}
}

func TestRun_HappyPath(t *testing.T) {
	cfgPath := setupRepo(t)
	top, err := config.LoadTop(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	srv := newTestServer(t, &hits)
	defer srv.Close()
	opts := testClient(t, srv)

	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("expected one /releases/latest call (cached for 2 arches), got %d", hits)
	}

	if got.SchemaVersion != plan.SchemaVersion {
		t.Errorf("schema_version: %d", got.SchemaVersion)
	}
	if got.DiscoveredAt != "2026-05-10T12:34:56Z" {
		t.Errorf("discovered_at: %s", got.DiscoveredAt)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("packages: %d", len(got.Packages))
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("result: %s err: %+v", pkg.Result, pkg.Error)
	}
	if pkg.Name != "hugo" {
		t.Errorf("name: %s", pkg.Name)
	}
	if pkg.Source == nil || pkg.Source.ReleaseID != 178213984 {
		t.Errorf("source: %+v", pkg.Source)
	}
	if pkg.Source.ReleasePublishedAt != "2026-05-09T08:12:00Z" {
		t.Errorf("published_at: %s", pkg.Source.ReleasePublishedAt)
	}
	if len(pkg.Artifacts) != 2 {
		t.Fatalf("artifacts: %d", len(pkg.Artifacts))
	}

	// amd64 first (sorted).
	a := pkg.Artifacts[0]
	if a.Arch != "amd64" {
		t.Errorf("first arch should be amd64 (sorted): %s", a.Arch)
	}
	if a.Asset.Name != "hugo_extended_0.140.0_linux-amd64.tar.gz" {
		t.Errorf("asset.name: %s", a.Asset.Name)
	}
	if a.Asset.SHA256 == nil || *a.Asset.SHA256 != "sha256:abc123" {
		t.Errorf("asset.sha256: %v", a.Asset.SHA256)
	}
	if a.Asset.SHA256Source == nil || *a.Asset.SHA256Source != plan.SHA256SourceGitHubAPI {
		t.Errorf("asset.sha256_source: %v", a.Asset.SHA256Source)
	}
	if a.Deb.Filename != "hugo_0.140.0_amd64.deb" {
		t.Errorf("deb.filename: %s", a.Deb.Filename)
	}

	// build_inputs_hash must verify.
	ok, recomputed, err := plan.VerifyBuildInputsHash(opts.Tool, a)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("build_inputs_hash mismatch: stored=%s recomputed=%s", a.Deb.BuildInputsHash, recomputed)
	}

	// BuildPlan.Nfpm has substituted ${VERSION}/${ARCH}, kept ${ASSETS}.
	var nfpmDecoded map[string]any
	if err := json.Unmarshal(a.BuildPlan.Nfpm, &nfpmDecoded); err != nil {
		t.Fatal(err)
	}
	if v, _ := nfpmDecoded["version"].(string); v != "0.140.0" {
		t.Errorf("nfpm.version: %v", nfpmDecoded["version"])
	}
	if v, _ := nfpmDecoded["arch"].(string); v != "amd64" {
		t.Errorf("nfpm.arch: %v", nfpmDecoded["arch"])
	}
	contents, _ := nfpmDecoded["contents"].([]any)
	first, _ := contents[0].(map[string]any)
	if src, _ := first["src"].(string); src != "${ASSETS}/hugo" {
		t.Errorf("first contents.src: %v", first["src"])
	}

	// aux_files: hugo.service inlined.
	aux, ok := a.BuildPlan.AuxFiles["./hugo.service"]
	if !ok {
		t.Fatalf("missing aux entry: %v", a.BuildPlan.AuxFiles)
	}
	body, _ := base64.StdEncoding.DecodeString(aux.ContentB64)
	if !strings.Contains(string(body), "Description=Hugo") {
		t.Errorf("aux body wrong: %s", body)
	}

	// SourceDateEpoch is the release published_at.Unix().
	want, _ := time.Parse(time.RFC3339, "2026-05-09T08:12:00Z")
	if a.BuildPlan.SourceDateEpoch != want.Unix() {
		t.Errorf("source_date_epoch: %d, want %d", a.BuildPlan.SourceDateEpoch, want.Unix())
	}

	// arm64 differs only in arch.
	b := pkg.Artifacts[1]
	if b.Arch != "arm64" {
		t.Errorf("second arch: %s", b.Arch)
	}
	if b.Deb.BuildInputsHash == a.Deb.BuildInputsHash {
		t.Errorf("amd64 and arm64 should have different hashes")
	}
}

func TestRun_AssetMismatch_BecomesPackageError(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(samplePackageBody,
		"hugo_extended_${VERSION}_linux-amd64.tar.gz",
		"hugo_extended_${VERSION}_linux-WRONG.tar.gz", 1)
	writeFile(t, filepath.Join(dir, "packages/hugo.yaml"), body)
	writeFile(t, filepath.Join(dir, "packages/hugo.service"), "x")
	writeFile(t, filepath.Join(dir, "cooper.yaml"), "packages: [./packages/hugo.yaml]\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, nil)
	defer srv.Close()
	opts := testClient(t, srv)

	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Packages[0].Result != plan.ResultError {
		t.Fatalf("expected error result, got %+v", got.Packages[0])
	}
	if got.Packages[0].Error.Kind != plan.ErrorKindDiscoveryFailed {
		t.Errorf("error.kind: %s", got.Packages[0].Error.Kind)
	}
}

func TestRun_PackageFilter(t *testing.T) {
	cfgPath := setupRepo(t)
	top, err := config.LoadTop(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, nil)
	defer srv.Close()
	opts := testClient(t, srv)
	opts.PackageFilter = map[string]bool{"not-hugo": true}

	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 0 {
		t.Errorf("filter should drop all packages: %v", got.Packages)
	}
}

func TestRun_GoldenJSONShape(t *testing.T) {
	cfgPath := setupRepo(t)
	top, err := config.LoadTop(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, nil)
	defer srv.Close()
	opts := testClient(t, srv)

	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: known keys present, deb.path / deb.sha256 are explicit nulls.
	for _, want := range []string{
		`"schema_version": 1`,
		`"format_revision": 1`,
		`"kind": "github_release"`,
		`"sha256_source": "github_api"`,
		`"path": null`,
		`"sha256": null`,
		`"build_inputs_hash":`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in JSON output", want)
		}
	}
}
