package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/giteaclient"
)

// newGiteaDiscoverer builds a discoverer pointed at a local httptest server.
// We construct the client with srv.URL as the Gitea base; the SDK appends
// /api/v1 + /repos/<owner>/<repo>/releases/... internally, so the test
// server sees that full path.
func newGiteaDiscoverer(t *testing.T, srv *httptest.Server, pattern string, includePre bool, token string) *GiteaReleaseDiscoverer {
	t.Helper()
	reg := NewRegistry(50, 4500)
	c, err := giteaclient.New(srv.Client(), srv.URL, token)
	if err != nil {
		t.Fatalf("giteaclient.New: %v", err)
	}
	d, err := NewGiteaReleaseDiscoverer("acme/widget", pattern, includePre, reg.BucketFor([]byte(token)), reg.CredentialID([]byte(token)), c)
	if err != nil {
		t.Fatalf("NewGiteaReleaseDiscoverer: %v", err)
	}
	return d
}

// giteaAttachJSON / giteaReleaseJSON mirror the Gitea SDK's struct shape
// with JSON tags matching what the API returns. Used to seed httptest
// responses without importing SDK-internal helpers.
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

func TestGitea_LatestRelease_HappyPath(t *testing.T) {
	body := giteaReleaseJSON{
		ID:      42,
		TagName: "v1.2.3",
		Attachments: []giteaAttachJSON{
			{ID: 1, Name: "widget_1.2.3_amd64.deb", Size: 12345, DownloadURL: "https://dl.example/widget.deb"},
			{ID: 2, Name: "widget_1.2.3_arm64.deb", Size: 23456, DownloadURL: "https://dl.example/widget-arm64.deb"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("ETag", `W/"gitea-abc"`)
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_.*_amd64\.deb`, false, "")
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Probe.URL != "https://dl.example/widget.deb" {
		t.Errorf("URL = %s", res.Probe.URL)
	}
	if res.Probe.Token != "42" {
		t.Errorf("Token = %s", res.Probe.Token)
	}
	if res.Probe.AssetSize != 12345 {
		t.Errorf("AssetSize = %d", res.Probe.AssetSize)
	}
	// Gitea attachments don't carry a digest, so the refresher does the
	// streaming-SHA path.
	if res.Probe.AssetDigest != "" {
		t.Errorf("expected empty digest, got %q", res.Probe.AssetDigest)
	}
	if res.Probe.APIEtag != `W/"gitea-abc"` {
		t.Errorf("ETag = %s", res.Probe.APIEtag)
	}
	if res.Unchanged {
		t.Errorf("expected Unchanged=false on cold start")
	}
}

func TestGitea_NotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"gitea-abc"` {
			t.Errorf("expected If-None-Match injected, got %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_amd64\.deb`, false, "")
	prev := &Probe{Token: "42", URL: "u", APIEtag: `W/"gitea-abc"`}
	res, err := d.Probe(context.Background(), ProbeInput{Prev: prev})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Unchanged {
		t.Fatal("expected Unchanged=true on 304")
	}
	if res.Probe.URL != "u" {
		t.Fatal("expected prev to be reused on 304")
	}
}

func TestGitea_AssetRegexMustMatchExactlyOne(t *testing.T) {
	body := giteaReleaseJSON{
		ID: 1, TagName: "v1",
		Attachments: []giteaAttachJSON{
			{Name: "widget_amd64.deb"},
			{Name: "widget_amd64.deb.sig"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_amd64.*`, false, "")
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on multiple matches")
	}
	if !strings.Contains(err.Error(), "matched 2 assets") {
		t.Fatalf("error did not surface match count: %v", err)
	}
}

func TestGitea_NoMatchIsHardError(t *testing.T) {
	body := giteaReleaseJSON{
		ID: 1, TagName: "v1",
		Attachments: []giteaAttachJSON{{Name: "widget_amd64.deb"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_arm64\.deb`, false, "")
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on zero matches")
	}
	if !strings.Contains(err.Error(), "no asset matching") {
		t.Fatalf("error did not surface zero-match: %v", err)
	}
}

func TestGitea_AuthHeaderInjected(t *testing.T) {
	body := giteaReleaseJSON{ID: 1, TagName: "v1", Attachments: []giteaAttachJSON{{Name: "x.deb", DownloadURL: "u"}}}
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `x\.deb`, false, "gtea_test")
	if _, err := d.Probe(context.Background(), ProbeInput{}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	auth := <-got
	// Gitea SDK injects "token <value>" rather than "Bearer <value>".
	if auth != "token gtea_test" {
		t.Fatalf("Authorization = %q, want token gtea_test", auth)
	}
}

func TestGitea_PrereleasesPath(t *testing.T) {
	releases := []giteaReleaseJSON{
		{ID: 2, TagName: "v2.0", IsPrerelease: false, Attachments: []giteaAttachJSON{{Name: "widget_2.0_amd64.deb", DownloadURL: "u"}}},
		{ID: 1, TagName: "v1.0", IsPrerelease: true, Attachments: []giteaAttachJSON{{Name: "widget_1.0_amd64.deb", DownloadURL: "u-old"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_.*_amd64\.deb`, true, "")
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Probe.Token != "2" {
		t.Errorf("expected newest release ID 2, got token %q", res.Probe.Token)
	}
}

func TestGitea_DraftsAreSkippedInListMode(t *testing.T) {
	// Drafts MUST be filtered even in the list-include-prereleases path —
	// gitea returns them in the same list endpoint.
	releases := []giteaReleaseJSON{
		{ID: 9, TagName: "draft", IsDraft: true, Attachments: []giteaAttachJSON{{Name: "widget_amd64.deb", DownloadURL: "u-draft"}}},
		{ID: 2, TagName: "v2.0", Attachments: []giteaAttachJSON{{Name: "widget_amd64.deb", DownloadURL: "u-real"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	d := newGiteaDiscoverer(t, srv, `widget_amd64\.deb`, true, "")
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Probe.URL != "u-real" {
		t.Errorf("draft leaked through: URL=%s", res.Probe.URL)
	}
}
