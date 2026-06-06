package source

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code.gitea.io/sdk/gitea"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
	"github.com/ophymx/apt-wharf/internal/giteaclient"
)

type giteaAttachJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

type giteaReleaseJSON struct {
	ID           int64             `json:"id"`
	TagName      string            `json:"tag_name"`
	IsDraft      bool              `json:"draft"`
	IsPrerelease bool              `json:"prerelease"`
	Attachments  []giteaAttachJSON `json:"assets"`
}

func newGiteaClient(t *testing.T, srv *httptest.Server) *gitea.Client {
	t.Helper()
	c, err := giteaclient.New(srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatalf("giteaclient.New: %v", err)
	}
	return c
}

func TestResolveGiteaRelease_Latest(t *testing.T) {
	body := giteaReleaseJSON{
		ID:      42,
		TagName: "v1.2.3",
		Attachments: []giteaAttachJSON{
			{Name: "widget_1.2.3_amd64.tar.gz", Size: 100, DownloadURL: "https://dl/widget.tgz"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	client := newGiteaClient(t, srv)

	gt := &config.GiteaSource{
		Server:  srv.URL,
		Repo:    "acme/widget",
		Release: &config.Release{Latest: true},
	}
	rel, err := ResolveGiteaRelease(client, gt)
	if err != nil {
		t.Fatal(err)
	}
	if rel.ID != 42 || rel.TagName != "v1.2.3" {
		t.Errorf("rel: %+v", rel)
	}
}

func TestResolveGiteaRelease_TagPattern(t *testing.T) {
	releases := []giteaReleaseJSON{
		{ID: 9, TagName: "nightly-2026-05-09"},
		{ID: 10, TagName: "v2.0.0-beta1", IsPrerelease: true},
		{ID: 11, TagName: "v1.4.0"},
		{ID: 12, TagName: "v1.3.5"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()
	client := newGiteaClient(t, srv)

	// mustSidecar runs through the strict YAML decoder + validate, which
	// memoizes the compiled tag_pattern regex on Release for downstream
	// use by firstMatchingGitea.
	pkg := mustSidecar(t, `
source:
  gitea:
    server: https://gitea.example.com
    repo: acme/widget
    release:
      tag_pattern: "^v\\d+\\.\\d+\\.\\d+$"
version_from: tag_strip_v
arches:
  amd64: { asset: x }
`)
	gt := pkg.Source.Gitea
	gt.Server = srv.URL // override so the test hits httptest, not the placeholder
	got, err := ResolveGiteaRelease(client, gt)
	if err != nil {
		t.Fatal(err)
	}
	// Tag pattern excludes nightly-* and v*-beta*; first match is v1.4.0 (ID 11).
	if got.ID != 11 {
		t.Errorf("expected v1.4.0 (ID 11), got %d %q", got.ID, got.TagName)
	}
}

func TestResolveGiteaRelease_ByTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/releases/tags/v1.4.0") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(giteaReleaseJSON{ID: 77, TagName: "v1.4.0"})
	}))
	defer srv.Close()
	client := newGiteaClient(t, srv)

	gt := &config.GiteaSource{
		Server:  srv.URL,
		Repo:    "acme/widget",
		Release: &config.Release{Tag: "v1.4.0"},
	}
	rel, err := ResolveGiteaRelease(client, gt)
	if err != nil {
		t.Fatal(err)
	}
	if rel.ID != 77 {
		t.Errorf("got ID %d", rel.ID)
	}
}

func TestMatchGiteaAsset_VersionSubstitution(t *testing.T) {
	rel := &gitea.Release{
		TagName: "v1.2.3",
		Attachments: []*gitea.Attachment{
			{Name: "widget_1.2.3_amd64.tar.gz", DownloadURL: "u1"},
			{Name: "widget_1.2.3_arm64.tar.gz", DownloadURL: "u2"},
		},
	}
	a, err := MatchGiteaAsset(rel, "widget_${VERSION}_amd64.tar.gz", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if a.DownloadURL != "u1" {
		t.Errorf("got %s", a.DownloadURL)
	}
}

func TestMatchGiteaAssets_RejectsDuplicateBasenames(t *testing.T) {
	rel := &gitea.Release{
		TagName: "v1",
		Attachments: []*gitea.Attachment{
			{Name: "widget.deb"},
		},
	}
	_, err := MatchGiteaAssets(rel, []string{"widget.deb", "widget.deb"}, "1")
	if err == nil || !strings.Contains(err.Error(), "asset name collision") {
		t.Fatalf("expected collision error, got %v", err)
	}
}
