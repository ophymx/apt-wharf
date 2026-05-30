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

	"github.com/ophymx/apt-wharf/internal/cooper/config"
	signsource "github.com/ophymx/apt-wharf/internal/source"
	"github.com/ophymx/apt-wharf/pkg/plan"
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
	if len(a.Assets) != 1 {
		t.Fatalf("assets: got %d, want 1", len(a.Assets))
	}
	if a.Assets[0].Name != "hugo_extended_0.140.0_linux-amd64.tar.gz" {
		t.Errorf("asset.name: %s", a.Assets[0].Name)
	}
	if a.Assets[0].SHA256 == nil || *a.Assets[0].SHA256 != "sha256:abc123" {
		t.Errorf("asset.sha256: %v", a.Assets[0].SHA256)
	}
	if a.Assets[0].SHA256Source == nil || *a.Assets[0].SHA256Source != plan.SHA256SourceGitHubAPI {
		t.Errorf("asset.sha256_source: %v", a.Assets[0].SHA256Source)
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

const samplePackageBodyJSONURL = `---
source:
  json_url:
    url: "REPLACE_ME"
    version_path: "releases.0.version"
arches:
  amd64:
    asset_url: "https://download.example.invalid/foo-${VERSION}-linux-${ARCH}.tar.gz"
  arm64:
    asset_url: "https://download.example.invalid/foo-${VERSION}-linux-${ARCH}.tar.gz"
---
name: foo
version: ${VERSION}
arch: ${ARCH}
maintainer: "Ophymx <ops@ophymx.com>"
description: A json_url-sourced fixture
contents:
  - src: ${ASSETS}/foo
    dst: /usr/bin/foo
`

func TestRun_JSONURL_HappyPath(t *testing.T) {
	// Spin up an httptest server that returns vendor-style JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[{"version":"1.2.3"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	body := strings.Replace(samplePackageBodyJSONURL, "REPLACE_ME", srv.URL, 1)
	writeFile(t, filepath.Join(dir, "packages/foo.yaml"), body)
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/foo.yaml\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Tool:       plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		HTTPClient: srv.Client(),
		Now: func() time.Time {
			t, _ := time.Parse(time.RFC3339, "2026-05-11T00:00:00Z")
			return t
		},
	}
	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("packages: %d", len(got.Packages))
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("result=%s err=%+v", pkg.Result, pkg.Error)
	}
	if pkg.Source == nil || pkg.Source.Kind != plan.SourceKindJSONURL {
		t.Errorf("source.kind: %+v", pkg.Source)
	}
	if pkg.Source.URL != srv.URL {
		t.Errorf("source.url: %q want %q", pkg.Source.URL, srv.URL)
	}
	if pkg.Source.Token != "1.2.3" {
		t.Errorf("source.token: %q", pkg.Source.Token)
	}
	if len(pkg.Artifacts) != 2 {
		t.Fatalf("artifacts: %d", len(pkg.Artifacts))
	}

	// amd64 first (sorted).
	a := pkg.Artifacts[0]
	if a.Arch != "amd64" {
		t.Errorf("arch[0]: %s", a.Arch)
	}
	wantURL := "https://download.example.invalid/foo-1.2.3-linux-amd64.tar.gz"
	if len(a.Assets) != 1 {
		t.Fatalf("assets: %d, want 1", len(a.Assets))
	}
	if a.Assets[0].URL != wantURL {
		t.Errorf("asset.url: %q want %q", a.Assets[0].URL, wantURL)
	}
	if a.Assets[0].Name != "foo-1.2.3-linux-amd64.tar.gz" {
		t.Errorf("asset.name: %q", a.Assets[0].Name)
	}
	if a.Assets[0].SHA256 != nil {
		t.Errorf("asset.sha256: %v, want nil (build will stream+hash)", a.Assets[0].SHA256)
	}
	if a.Deb.Filename != "foo_1.2.3_amd64.deb" {
		t.Errorf("deb.filename: %s", a.Deb.Filename)
	}

	// build_inputs_hash recomputes.
	ok, recomputed, err := plan.VerifyBuildInputsHash(opts.Tool, a)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("build_inputs_hash mismatch: stored=%s recomputed=%s", a.Deb.BuildInputsHash, recomputed)
	}

	// SourceDateEpoch is derived deterministically; pin the 2020-base
	// window and assert stability across two discover calls.
	const baseEpoch = int64(1577836800)
	const span = int64(10 * 365 * 24 * 3600)
	if a.BuildPlan.SourceDateEpoch < baseEpoch || a.BuildPlan.SourceDateEpoch >= baseEpoch+span {
		t.Errorf("source_date_epoch %d outside derived window", a.BuildPlan.SourceDateEpoch)
	}

	// Determinism: a second discover against the same JSON yields the
	// same hash (idempotency property — same plan → same bytes).
	got2, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	a2 := got2.Packages[0].Artifacts[0]
	if a.Deb.BuildInputsHash != a2.Deb.BuildInputsHash {
		t.Errorf("hash drifted across runs: %s vs %s", a.Deb.BuildInputsHash, a2.Deb.BuildInputsHash)
	}
}

const samplePackageBodyXMLURL = `---
source:
  xml_url:
    url: "REPLACE_ME"
    version_xpath: "//channel[@status='release']/build/@number"
arches:
  amd64:
    asset_url: "https://download.example.invalid/foo-${VERSION}-linux-${ARCH}.tar.gz"
  arm64:
    asset_url: "https://download.example.invalid/foo-${VERSION}-linux-${ARCH}.tar.gz"
---
name: foo
version: ${VERSION}
arch: ${ARCH}
maintainer: "Ophymx <ops@ophymx.com>"
description: An xml_url-sourced fixture
contents:
  - src: ${ASSETS}/foo
    dst: /usr/bin/foo
`

const xmlUpdatesBody = `<?xml version="1.0" encoding="UTF-8"?>
<products>
  <product name="Foo">
    <channel id="release" status="release">
      <build number="1.2.3" fullNumber="1.2.3.45"/>
    </channel>
  </product>
</products>`

func TestRun_XMLURL_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(xmlUpdatesBody))
	}))
	defer srv.Close()

	dir := t.TempDir()
	body := strings.Replace(samplePackageBodyXMLURL, "REPLACE_ME", srv.URL, 1)
	writeFile(t, filepath.Join(dir, "packages/foo.yaml"), body)
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/foo.yaml\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Tool:       plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		HTTPClient: srv.Client(),
		Now: func() time.Time {
			t, _ := time.Parse(time.RFC3339, "2026-05-11T00:00:00Z")
			return t
		},
	}
	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("packages: %d", len(got.Packages))
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("result=%s err=%+v", pkg.Result, pkg.Error)
	}
	if pkg.Source == nil || pkg.Source.Kind != plan.SourceKindXMLURL {
		t.Errorf("source.kind: %+v", pkg.Source)
	}
	if pkg.Source.Token != "1.2.3" {
		t.Errorf("source.token: %q", pkg.Source.Token)
	}
	if len(pkg.Artifacts) != 2 {
		t.Fatalf("artifacts: %d", len(pkg.Artifacts))
	}

	a := pkg.Artifacts[0]
	wantURL := "https://download.example.invalid/foo-1.2.3-linux-amd64.tar.gz"
	if len(a.Assets) != 1 || a.Assets[0].URL != wantURL {
		t.Errorf("assets[0].url: %v want %q", a.Assets, wantURL)
	}
	if a.Deb.Filename != "foo_1.2.3_amd64.deb" {
		t.Errorf("deb.filename: %s", a.Deb.Filename)
	}

	ok, recomputed, err := plan.VerifyBuildInputsHash(opts.Tool, a)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("build_inputs_hash mismatch: stored=%s recomputed=%s", a.Deb.BuildInputsHash, recomputed)
	}

	got2, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	a2 := got2.Packages[0].Artifacts[0]
	if a.Deb.BuildInputsHash != a2.Deb.BuildInputsHash {
		t.Errorf("hash drifted across runs: %s vs %s", a.Deb.BuildInputsHash, a2.Deb.BuildInputsHash)
	}
}

func TestRun_JSONURL_InvalidVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Version that doesn't match Debian grammar (starts with letter).
		_, _ = w.Write([]byte(`{"releases":[{"version":"latest"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	body := strings.Replace(samplePackageBodyJSONURL, "REPLACE_ME", srv.URL, 1)
	writeFile(t, filepath.Join(dir, "packages/foo.yaml"), body)
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/foo.yaml\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Tool:       plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		HTTPClient: srv.Client(),
	}
	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultError || pkg.Error.Kind != plan.ErrorKindVersionInvalid {
		t.Errorf("expected version_invalid error, got result=%s err=%+v", pkg.Result, pkg.Error)
	}
}

const externalPackageBody = `---
source:
  external:
    command: ["./discover.sh"]
arches:
  amd64:
    asset_url: "https://example.invalid/foo-${VERSION}-${ARCH}.tar.gz"
  arm64:
    asset_url: "https://example.invalid/foo-${VERSION}-${ARCH}.tar.gz"
---
name: foo
version: ${VERSION}
arch: ${ARCH}
maintainer: "Ophymx <ops@ophymx.com>"
description: External-source test package
contents:
  - src: ${ASSETS}/foo
    dst: /usr/bin/foo
`

const externalDiscoverScript = `#!/bin/sh
cat <<'EOF'
{
  "version": "1.2.3",
  "assets": [
    { "arch": "amd64", "url": "https://example.invalid/foo-1.2.3-amd64.tar.gz", "sha256": "0000000000000000000000000000000000000000000000000000000000000001" },
    { "arch": "arm64", "url": "https://example.invalid/foo-1.2.3-arm64.tar.gz" }
  ]
}
EOF
`

func TestRun_External(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "packages/foo.yaml"), externalPackageBody)
	writeFile(t, filepath.Join(dir, "packages/discover.sh"), externalDiscoverScript)
	if err := os.Chmod(filepath.Join(dir, "packages/discover.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Aux file referenced by the nfpm doc — not actually needed for an
	// external source, but discover walks aux refs unconditionally.
	writeFile(t, filepath.Join(dir, "packages/foo"), "binary contents")
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/foo.yaml\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Tool: plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision},
		Now: func() time.Time {
			t, _ := time.Parse(time.RFC3339, "2026-05-10T12:34:56Z")
			return t
		},
	}

	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("packages: %d", len(got.Packages))
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultOK {
		t.Fatalf("result: %s err: %+v", pkg.Result, pkg.Error)
	}
	if pkg.Source == nil || pkg.Source.Kind != plan.SourceKindExternal {
		t.Errorf("source: %+v", pkg.Source)
	}
	if pkg.Source.Token != "1.2.3" {
		t.Errorf("source.token: %s", pkg.Source.Token)
	}
	if pkg.Source.URL != "./discover.sh" {
		t.Errorf("source.url: %s", pkg.Source.URL)
	}
	if len(pkg.Artifacts) != 2 {
		t.Fatalf("artifacts: %d", len(pkg.Artifacts))
	}

	// amd64 first (sorted): has script-provided SHA256.
	amd64 := pkg.Artifacts[0]
	if amd64.Arch != "amd64" {
		t.Errorf("artifacts[0].arch: %s", amd64.Arch)
	}
	if got := amd64.Assets[0].URL; got != "https://example.invalid/foo-1.2.3-amd64.tar.gz" {
		t.Errorf("amd64 url: %s", got)
	}
	if amd64.Assets[0].SHA256 == nil || *amd64.Assets[0].SHA256 != "sha256:0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("amd64 sha256: %v", amd64.Assets[0].SHA256)
	}
	if amd64.Assets[0].SHA256Source == nil || *amd64.Assets[0].SHA256Source != plan.SHA256SourceExternalScript {
		t.Errorf("amd64 sha256_source: %v", amd64.Assets[0].SHA256Source)
	}
	if amd64.Deb.Filename != "foo_1.2.3_amd64.deb" {
		t.Errorf("amd64 deb filename: %s", amd64.Deb.Filename)
	}

	// arm64: no SHA256 (script omitted it).
	arm64 := pkg.Artifacts[1]
	if arm64.Assets[0].SHA256 != nil {
		t.Errorf("arm64 sha256 should be nil, got %v", *arm64.Assets[0].SHA256)
	}

	// build_inputs_hash recomputes cleanly for the amd64 artifact.
	ok, recomputed, err := plan.VerifyBuildInputsHash(opts.Tool, amd64)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("amd64 hash recompute mismatch: stored=%s recomputed=%s",
			amd64.Deb.BuildInputsHash, recomputed)
	}
}

func TestRun_External_URLDriftRejected(t *testing.T) {
	dir := t.TempDir()
	// Recipe pins the URL shape; script returns a URL that doesn't fit.
	pkgBody := `---
source:
  external:
    command: ["./discover.sh"]
arches:
  amd64:
    asset_url: "https://example.invalid/foo-${VERSION}-${ARCH}.tar.gz"
---
name: foo
version: ${VERSION}
arch: ${ARCH}
maintainer: "Ophymx <ops@ophymx.com>"
description: drift test
contents:
  - src: ${ASSETS}/foo
    dst: /usr/bin/foo
`
	scriptBody := `#!/bin/sh
echo '{"version":"1.2.3","assets":[{"arch":"amd64","url":"https://example.invalid/MISMATCH-1.2.3.tar.gz"}]}'
`
	writeFile(t, filepath.Join(dir, "packages/foo.yaml"), pkgBody)
	writeFile(t, filepath.Join(dir, "packages/discover.sh"), scriptBody)
	if err := os.Chmod(filepath.Join(dir, "packages/discover.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "packages/foo"), "x")
	writeFile(t, filepath.Join(dir, "cooper.yaml"), "packages:\n  - ./packages/foo.yaml\n")

	top, err := config.LoadTop(filepath.Join(dir, "cooper.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Tool: plan.Tool{Name: "cooper", Version: "0.0.0-test", FormatRevision: plan.FormatRevision}}
	got, err := Run(context.Background(), top, opts)
	if err != nil {
		t.Fatal(err)
	}
	pkg := got.Packages[0]
	if pkg.Result != plan.ResultError {
		t.Fatalf("expected drift-validation error, got result=%s", pkg.Result)
	}
	if !strings.Contains(pkg.Error.Message, "does not match recipe template") {
		t.Errorf("error message: %s", pkg.Error.Message)
	}
}
