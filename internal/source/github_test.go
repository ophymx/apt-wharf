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
)

// newDiscoverer builds a discoverer pointed at a local httptest server.
// The go-github client's BaseURL is rewritten to the test server's URL so
// no real GitHub traffic happens.
func newDiscoverer(t *testing.T, srv *httptest.Server, pattern string, includePre bool, token []byte) *GitHubReleaseDiscoverer {
	t.Helper()
	reg := NewRegistry(50, 4500)
	client := NewGitHubClient(srv.Client(), token)
	base, _ := url.Parse(srv.URL + "/")
	client.BaseURL = base
	d, err := NewGitHubReleaseDiscoverer("acme/widget", pattern, includePre, reg.BucketFor(token), reg.CredentialID(token), client)
	if err != nil {
		t.Fatalf("NewGitHubReleaseDiscoverer: %v", err)
	}
	return d
}

// ghAssetJSON / ghReleaseJSON mirror the go-github structs shape with
// JSON tags matching what the API returns. Using these avoids importing
// internal go-github JSON helpers.
type ghAssetJSON struct {
	Name               string `json:"name"`
	Size               int    `json:"size"`
	Digest             string `json:"digest,omitempty"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghReleaseJSON struct {
	ID         int64         `json:"id"`
	TagName    string        `json:"tag_name"`
	Prerelease bool          `json:"prerelease"`
	Assets     []ghAssetJSON `json:"assets"`
}

func TestGitHub_LatestRelease_HappyPath(t *testing.T) {
	body := ghReleaseJSON{
		ID:      42,
		TagName: "v1.2.3",
		Assets: []ghAssetJSON{
			{Name: "widget_1.2.3_amd64.deb", Size: 12345, Digest: "sha256:abcdef", BrowserDownloadURL: "https://dl.example/widget.deb"},
			{Name: "widget_1.2.3_arm64.deb", Size: 23456, Digest: "sha256:fedcba", BrowserDownloadURL: "https://dl.example/widget-arm64.deb"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases/latest") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("ETag", `W/"abc"`)
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `widget_.*_amd64\.deb`, false, nil)
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
	if res.Probe.AssetDigest != "abcdef" {
		t.Errorf("Digest = %s", res.Probe.AssetDigest)
	}
	if res.Probe.APIEtag != `W/"abc"` {
		t.Errorf("ETag = %s", res.Probe.APIEtag)
	}
	if res.Unchanged {
		t.Errorf("expected Unchanged=false on cold start")
	}
}

func TestGitHub_NotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"abc"` {
			t.Errorf("expected If-None-Match injected, got %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `widget_amd64\.deb`, false, nil)
	prev := &Probe{Token: "42", URL: "u", APIEtag: `W/"abc"`}
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

func TestGitHub_AssetRegexMustMatchExactlyOne(t *testing.T) {
	body := ghReleaseJSON{
		ID:      1,
		TagName: "v1",
		Assets: []ghAssetJSON{
			{Name: "widget_amd64.deb"},
			{Name: "widget_amd64.deb.sig"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `widget_amd64.*`, false, nil)
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on multiple matches")
	}
	if !strings.Contains(err.Error(), "matched 2 assets") {
		t.Fatalf("error did not surface match count: %v", err)
	}
}

func TestGitHub_NoMatchIsHardError(t *testing.T) {
	body := ghReleaseJSON{
		ID: 1, TagName: "v1",
		Assets: []ghAssetJSON{{Name: "widget_amd64.deb"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `widget_arm64\.deb`, false, nil)
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on zero matches")
	}
	if !strings.Contains(err.Error(), "no asset matching") {
		t.Fatalf("error did not surface zero-match: %v", err)
	}
}

func TestGitHub_RateLimitBlocksOnReset(t *testing.T) {
	body := ghReleaseJSON{
		ID: 1, TagName: "v1",
		Assets: []ghAssetJSON{{Name: "x.deb", BrowserDownloadURL: "u"}},
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "9999999999") // far future
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	reg := NewRegistry(50, 4500)
	client := NewGitHubClient(srv.Client(), nil)
	base, _ := url.Parse(srv.URL + "/")
	client.BaseURL = base
	d, err := NewGitHubReleaseDiscoverer("acme/widget", `x\.deb`, false, reg.BucketFor(nil), reg.CredentialID(nil), client)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Probe(context.Background(), ProbeInput{}); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	_, err = d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected rate limit error on second probe")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("error not rate-limit related: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls)
	}
}

func TestGitHub_AuthHeaderInjected(t *testing.T) {
	body := ghReleaseJSON{ID: 1, TagName: "v1", Assets: []ghAssetJSON{{Name: "x.deb", BrowserDownloadURL: "u"}}}
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `x\.deb`, false, []byte("ghp_test"))
	if _, err := d.Probe(context.Background(), ProbeInput{}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	auth := <-got
	if auth != "Bearer ghp_test" {
		t.Fatalf("Authorization = %q, want Bearer ghp_test", auth)
	}
}

func TestRegistry_TokenSharing(t *testing.T) {
	r := NewRegistry(50, 4500)
	tokA := []byte("alpha-token")
	tokB := []byte("alpha-token")
	tokC := []byte("beta-token")

	a := r.BucketFor(tokA)
	b := r.BucketFor(tokB)
	c := r.BucketFor(tokC)
	un := r.BucketFor(nil)

	if a != b {
		t.Fatal("identical tokens must share a bucket")
	}
	if a == c || a == un {
		t.Fatal("different tokens must not share a bucket")
	}
}

func TestGitHub_PrereleasesPath(t *testing.T) {
	releases := []ghReleaseJSON{
		{ID: 2, TagName: "v2.0", Prerelease: false, Assets: []ghAssetJSON{{Name: "widget_2.0_amd64.deb", BrowserDownloadURL: "u"}}},
		{ID: 1, TagName: "v1.0", Prerelease: true, Assets: []ghAssetJSON{{Name: "widget_1.0_amd64.deb", BrowserDownloadURL: "u-old"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/releases") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	d := newDiscoverer(t, srv, `widget_.*_amd64\.deb`, true, nil)
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// Newest release wins — go-github returns them newest-first.
	if res.Probe.Token != "2" {
		t.Errorf("expected newest release ID 2, got token %q", res.Probe.Token)
	}
}

// Compile-time check that github.RepositoryRelease is what we expect.
var _ = (*github.RepositoryRelease)(nil)
