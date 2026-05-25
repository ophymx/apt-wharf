package refresh

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/config"
	"github.com/ophymx/apt-wharf/internal/fetch"
	"github.com/ophymx/apt-wharf/internal/sign"
	"github.com/ophymx/apt-wharf/internal/source"
	"github.com/ophymx/apt-wharf/internal/store"
)

// TestRefresh_BootstrapVersionStableAcrossTicks pins down the user-visible
// bug: "apt continually thinks it can upgrade the bootstrap." Root cause
// candidate is the bootstrap rebuilding (and version-bumping) on every
// refresh tick when nothing meaningful changed.
func TestRefresh_BootstrapVersionStableAcrossTicks(t *testing.T) {
	debBytes := buildFakeDeb(t, "widget", "1.0.0", "amd64")
	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(t, w, r, debBytes)
	}))
	defer assetSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"r1"`)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "tag_name": "v1.0.0",
			"assets": []map[string]any{{
				"name": "widget_1.0.0_amd64.deb", "size": len(debBytes),
				"browser_download_url": assetSrv.URL + "/widget.deb",
			}},
		})
	}))
	defer apiSrv.Close()

	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "secring.gpg")
	if err := sign.EnsureKey(keyPath, "Local <local@localhost>"); err != nil {
		t.Fatal(err)
	}
	signer, err := sign.Load(keyPath, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(tmp, "state")
	st := store.New(stateDir)
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	cfg := &config.Config{
		Repository: config.Repository{Origin: "L", Label: "L", BaseURL: "http://localhost:8080"},
		Suite:      config.Suite{Codename: "stable", Description: "L", Architectures: []string{"amd64"}},
		Bootstrap:  config.Bootstrap{PackageName: "local-archive-keyring", Maintainer: "L <l@l>", Description: "L"},
		Refresh:    config.Refresh{Interval: config.Duration(time.Hour), HTTPTimeout: config.Duration(30 * time.Second)},
		GitHub:     config.GitHub{RateLimit: config.RateLimit{UnauthenticatedPerHour: 50, AuthenticatedPerHour: 4500}},
		Paths:      config.Paths{StateDir: stateDir},
		Sources: map[string]*config.Source{
			"widget": {Discovery: config.Discovery{Type: "github_release", Repo: "acme/widget", Asset: `widget_.*_amd64\.deb`}},
		},
	}
	registry := source.NewRegistry(50, 4500)
	gh := source.NewGitHubClient(httpClient, nil)
	base, _ := url.Parse(apiSrv.URL + "/")
	gh.BaseURL = base
	disc, err := source.NewGitHubReleaseDiscoverer("acme/widget", `widget_.*_amd64\.deb`, false,
		registry.BucketFor(nil), registry.CredentialID(nil), gh)
	if err != nil {
		t.Fatal(err)
	}

	rf := New(Options{
		Cfg: cfg, Signer: signer, Store: st,
		Fetcher:     fetch.New(httpClient),
		HTTPClient:  httpClient,
		Discoverers: map[string]source.Discoverer{"widget": disc},
		Holder:      &Holder{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx := context.Background()
	if err := rf.SyncImportNew(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rf.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := st.LoadBootstrap()
	if err != nil {
		t.Fatal(err)
	}

	// Second tick — nothing changed.
	if err := rf.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := st.LoadBootstrap()
	if err != nil {
		t.Fatal(err)
	}

	if first.Version != second.Version {
		t.Errorf("bootstrap version bumped between identical ticks: %s -> %s",
			first.Version, second.Version)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("bootstrap .deb SHA256 changed between identical ticks: %s -> %s",
			first.SHA256, second.SHA256)
	}
	if first.InputHash != second.InputHash {
		t.Errorf("bootstrap input hash changed between identical ticks: %s -> %s",
			first.InputHash, second.InputHash)
	}

	// And the snapshot's published Packages stanza must reference the same
	// version + sha256 as the .deb itself.
	snap := rf.Snapshot()
	pkg := snap.Files["/dists/stable/main/binary-amd64/Packages"]
	if !strings.Contains(string(pkg.Data), "Version: "+first.Version) {
		t.Errorf("Packages does not reference bootstrap version %s\n%s", first.Version, pkg.Data)
	}
	if !strings.Contains(string(pkg.Data), "SHA256: "+first.SHA256) {
		t.Errorf("Packages does not reference bootstrap SHA256 %s\n%s", first.SHA256, pkg.Data)
	}
}
