package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
	signsource "github.com/ophymx/apt-signpost/internal/source"
)

type ghAssetJSON struct {
	Name               string `json:"name"`
	Size               int    `json:"size"`
	Digest             string `json:"digest,omitempty"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghReleaseJSON struct {
	ID         int64         `json:"id"`
	TagName    string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []ghAssetJSON `json:"assets"`
}

// newClient wires a go-github *github.Client at a local httptest server.
func newClient(t *testing.T, srv *httptest.Server) *github.Client {
	t.Helper()
	client := signsource.NewGitHubClient(srv.Client(), nil)
	base, _ := url.Parse(srv.URL + "/")
	client.BaseURL = base
	return client
}

func releaseHandler(t *testing.T, body any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode: %v", err)
		}
	}
}

func TestResolveRelease_Latest(t *testing.T) {
	body := ghReleaseJSON{
		ID:      42,
		TagName: "v1.2.3",
		Assets: []ghAssetJSON{
			{Name: "widget_1.2.3_amd64.tar.gz", Size: 100, Digest: "sha256:abc"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	client := newClient(t, srv)

	gh := &config.GitHubSource{
		Repo:    "acme/widget",
		Release: &config.Release{Latest: true},
	}
	rel, err := ResolveRelease(context.Background(), client, gh)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 42 || rel.GetTagName() != "v1.2.3" {
		t.Errorf("rel: %+v", rel)
	}
}

func TestResolveRelease_LatestIncludingPrerelease(t *testing.T) {
	releases := []ghReleaseJSON{
		{ID: 1, TagName: "v2.0.0-rc1", Prerelease: true},
		{ID: 2, TagName: "v1.2.3"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repos/acme/widget/releases") || strings.HasSuffix(r.URL.Path, "/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()
	client := newClient(t, srv)

	gh := &config.GitHubSource{
		Repo:              "acme/widget",
		Release:           &config.Release{Latest: true},
		IncludePrerelease: true,
	}
	rel, err := ResolveRelease(context.Background(), client, gh)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 1 {
		t.Errorf("expected first release (rc1, ID 1), got %d", rel.GetID())
	}
}

func TestResolveRelease_TagPattern(t *testing.T) {
	releases := []ghReleaseJSON{
		{ID: 9, TagName: "nightly-2026-05-09"},
		{ID: 10, TagName: "v2.0.0-beta1", Prerelease: true},
		{ID: 11, TagName: "v1.4.0"},
		{ID: 12, TagName: "v1.3.5"},
	}
	srv := httptest.NewServer(releaseHandler(t, releases))
	defer srv.Close()
	client := newClient(t, srv)

	pkg := mustSidecar(t, `
source:
  github:
    repo: acme/widget
    release:
      tag_pattern: "^v\\d+\\.\\d+\\.\\d+$"
version_from: tag_strip_v
arches:
  amd64: { asset: x }
`)
	rel, err := ResolveRelease(context.Background(), client, pkg.Source.GitHub)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 11 {
		t.Errorf("expected v1.4.0 (ID 11), got tag %s id %d", rel.GetTagName(), rel.GetID())
	}
}

func TestResolveRelease_TagPattern_SkipsPrereleaseUnlessOptIn(t *testing.T) {
	releases := []ghReleaseJSON{
		{ID: 10, TagName: "v2.0.0-beta1", Prerelease: true},
		{ID: 11, TagName: "v1.4.0"},
	}
	srv := httptest.NewServer(releaseHandler(t, releases))
	defer srv.Close()
	client := newClient(t, srv)

	// Default: skip prerelease.
	pkg := mustSidecar(t, `
source:
  github:
    repo: acme/widget
    release:
      tag_pattern: "^v"
version_from: tag
arches: { amd64: { asset: x } }
`)
	rel, err := ResolveRelease(context.Background(), client, pkg.Source.GitHub)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 11 {
		t.Errorf("expected v1.4.0 when prerelease excluded, got %s", rel.GetTagName())
	}

	// Opt in: include prerelease.
	pkg2 := mustSidecar(t, `
source:
  github:
    repo: acme/widget
    release:
      tag_pattern: "^v"
    include_prerelease: true
version_from: tag
arches: { amd64: { asset: x } }
`)
	rel, err = ResolveRelease(context.Background(), client, pkg2.Source.GitHub)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 10 {
		t.Errorf("expected v2.0.0-beta1 when prerelease included, got %s", rel.GetTagName())
	}
}

func TestResolveRelease_Tag(t *testing.T) {
	body := ghReleaseJSON{ID: 77, TagName: "v0.9.7"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases/tags/v0.9.7") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	client := newClient(t, srv)

	gh := &config.GitHubSource{
		Repo:    "acme/widget",
		Release: &config.Release{Tag: "v0.9.7"},
	}
	rel, err := ResolveRelease(context.Background(), client, gh)
	if err != nil {
		t.Fatal(err)
	}
	if rel.GetID() != 77 {
		t.Errorf("rel: %+v", rel)
	}
}

func TestResolveRelease_NoMatch(t *testing.T) {
	srv := httptest.NewServer(releaseHandler(t, []ghReleaseJSON{
		{ID: 1, TagName: "main", Prerelease: true},
	}))
	defer srv.Close()
	client := newClient(t, srv)

	pkg := mustSidecar(t, `
source:
  github:
    repo: acme/widget
    release: { tag_pattern: "^v\\d" }
version_from: tag
arches: { amd64: { asset: x } }
`)
	_, err := ResolveRelease(context.Background(), client, pkg.Source.GitHub)
	if err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("expected no-release error, got %v", err)
	}
}

func TestMatchAsset(t *testing.T) {
	rel := &github.RepositoryRelease{
		TagName: stringPtr("v1.2.3"),
		Assets: []*github.ReleaseAsset{
			ghAsset("widget_1.2.3_linux-amd64.tar.gz", "sha256:aa"),
			ghAsset("widget_1.2.3_linux-arm64.tar.gz", "sha256:bb"),
			ghAsset("widget_1.2.3_windows-amd64.zip", "sha256:cc"),
		},
	}
	a, err := MatchAsset(rel, "widget_${VERSION}_linux-amd64.tar.gz", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if a.GetName() != "widget_1.2.3_linux-amd64.tar.gz" {
		t.Errorf("got %s", a.GetName())
	}
	hex, ok := AssetSHA256(a)
	if !ok || hex != "aa" {
		t.Errorf("AssetSHA256: ok=%v hex=%s", ok, hex)
	}
}

func TestMatchAsset_NoMatch(t *testing.T) {
	rel := &github.RepositoryRelease{
		TagName: stringPtr("v1.2.3"),
		Assets:  []*github.ReleaseAsset{ghAsset("other.tgz", "")},
	}
	_, err := MatchAsset(rel, "widget_${VERSION}.tar.gz", "1.2.3")
	if err == nil || !strings.Contains(err.Error(), "no asset named widget_1.2.3.tar.gz") {
		t.Fatalf("expected no-asset error, got %v", err)
	}
}

func TestMatchAsset_AmbiguousRejected(t *testing.T) {
	rel := &github.RepositoryRelease{
		TagName: stringPtr("v1.2.3"),
		Assets: []*github.ReleaseAsset{
			ghAsset("dup.tgz", ""),
			ghAsset("dup.tgz", ""),
		},
	}
	_, err := MatchAsset(rel, "dup.tgz", "1.2.3")
	if err == nil || !strings.Contains(err.Error(), "matched 2 assets") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestAssetSHA256_NonSha256Algorithm(t *testing.T) {
	a := ghAsset("x", "sha512:deadbeef")
	if _, ok := AssetSHA256(a); ok {
		t.Error("expected ok=false for non-sha256 algorithm")
	}
}

// mustSidecar parses a one-doc YAML body as a Sidecar (uses the config
// package's strict decoder via a synthetic two-doc file).
func mustSidecar(t *testing.T, body string) config.Sidecar {
	t.Helper()
	full := body + "\n---\nname: test\n"
	dir := t.TempDir()
	p := dir + "/x.yaml"
	if err := writeFile(p, full); err != nil {
		t.Fatal(err)
	}
	pkg, err := config.LoadPackage(p)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	return pkg.Sidecar
}

func writeFile(path, body string) error {
	return writeAll(path, body)
}

// writeAll is in a separate file so we can override behavior in tests
// later if needed.
