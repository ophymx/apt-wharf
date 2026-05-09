//go:build integration

// Live-network integration tests against real GitHub releases. Run with:
//
//	go test -tags integration ./internal/source/...
//
// Two fixtures cover both architecture code paths (per design-mvp.md):
//   - goreleaser/nfpm        → arch-specific .deb (Architecture: amd64)
//   - vkbo/novelwriter       → Architecture: all
//
// Set GITHUB_TOKEN to avoid running against the 60-req/h unauthenticated
// budget (still works without one; just be aware).
package source

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/internal/fetch"
	"github.com/ophymx/apt-signpost/internal/index"
)

func liveDiscoverer(t *testing.T, repo, pattern string, includePre bool) *GitHubReleaseDiscoverer {
	t.Helper()
	var token []byte
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		token = []byte(v)
	}
	reg := NewRegistry(50, 4500)
	httpClient := &http.Client{Timeout: 60 * time.Second}
	client := NewGitHubClient(httpClient, token)
	d, err := NewGitHubReleaseDiscoverer(repo, pattern, includePre,
		reg.BucketFor(token), reg.CredentialID(token), client)
	if err != nil {
		t.Fatalf("NewGitHubReleaseDiscoverer: %v", err)
	}
	return d
}

func liveFetchAndParse(t *testing.T, d *GitHubReleaseDiscoverer) (string, string, string) {
	t.Helper()
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := d.Probe(ctx, ProbeInput{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("probed: tag=%s url=%s digest=%s", res.Probe.ReleaseTag, res.Probe.URL, res.Probe.AssetDigest)

	f := fetch.New(httpClient)
	fr, err := f.Fetch(ctx, fetch.Options{URL: res.Probe.URL, NeedHash: res.Probe.AssetDigest == ""})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	fields, err := index.ExtractFields(fr.Control)
	if err != nil {
		t.Fatalf("ExtractFields: %v", err)
	}
	t.Logf("control: pkg=%s ver=%s arch=%s size=%d sha256=%s",
		fields.Package, fields.Version, fields.Architecture, fr.Size, fr.SHA256)
	return fields.Package, fields.Version, fields.Architecture
}

func TestLive_Nfpm_ArchSpecific(t *testing.T) {
	// nfpm publishes arch-specific .debs as nfpm_<version>_<arch>.deb.
	// We pick amd64 and assert the parsed Architecture matches.
	d := liveDiscoverer(t, "goreleaser/nfpm", `nfpm_[0-9.]+_amd64\.deb`, false)
	pkg, _, arch := liveFetchAndParse(t, d)
	if pkg != "nfpm" {
		t.Errorf("Package = %q, want nfpm", pkg)
	}
	if arch != "amd64" {
		t.Errorf("Architecture = %q, want amd64", arch)
	}
}

func TestLive_Novelwriter_ArchAll(t *testing.T) {
	// novelwriter publishes one stable + one oldstable .deb (both
	// Architecture: all). Anchored regex selects exactly the stable one.
	d := liveDiscoverer(t, "vkbo/novelwriter", `novelwriter_[0-9.]+_all\.deb`, false)
	pkg, _, arch := liveFetchAndParse(t, d)
	if !strings.Contains(strings.ToLower(pkg), "novelwriter") {
		t.Errorf("Package = %q, expected novelwriter-ish", pkg)
	}
	if arch != "all" {
		t.Errorf("Architecture = %q, want all", arch)
	}
}
