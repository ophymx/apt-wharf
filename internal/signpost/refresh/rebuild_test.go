package refresh

import (
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/config"
	"github.com/ophymx/apt-wharf/internal/signpost/fetch"
	"github.com/ophymx/apt-wharf/internal/signpost/sign"
	"github.com/ophymx/apt-wharf/internal/signpost/source"
	"github.com/ophymx/apt-wharf/internal/signpost/store"
)

// TestRebuild_FromStateOnly composes a snapshot with no discoverers and
// no network, purely from a pre-seeded state dir — the `export --offline`
// path. The bootstrap .deb is still built (local nfpm, no network).
func TestRebuild_FromStateOnly(t *testing.T) {
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "secring.gpg")
	writePGPSecret(t, keyPath, newPGPEntity(t, "Test Signer"))
	signer, err := sign.Load(keyPath, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(tmp, "state")
	st := store.New(stateDir)
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteSource(&store.SourceState{
		Name:           "vendor-widget-amd64",
		DiscoveryToken: "r1",
		AssetURL:       "https://dl.example.com/widget_1.2.3_amd64.deb",
		AssetSize:      1234,
		AssetSHA256:    strings.Repeat("ab", 32),
		Control:        "Package: widget\nVersion: 1.2.3\nArchitecture: amd64\nMaintainer: x <x@example.com>\nDescription: widget\n",
		LastChecked:    time.Now(),
		LastChanged:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Repository: config.Repository{Origin: "Acme", Label: "Acme", BaseURL: "https://apt.acme.example"},
		Suite:      config.Suite{Codename: "stable", Description: "d", Architectures: []string{"amd64"}},
		Bootstrap:  config.Bootstrap{PackageName: "acme-archive-keyring", Maintainer: "Ops <ops@acme.example>", Description: "k"},
		Paths:      config.Paths{StateDir: stateDir},
	}
	httpClient := &http.Client{Timeout: time.Second}
	holder := &Holder{}
	rf := New(Options{
		Cfg: cfg, Signer: signer, Store: st, Fetcher: fetch.New(httpClient), HTTPClient: httpClient,
		Discoverers: map[string]source.Discoverer{}, Holder: holder,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := rf.Rebuild(); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := holder.Load()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	rd, ok := snap.Redirects["/pool/main/w/widget/widget_1.2.3_amd64.deb"]
	if !ok || rd.URL != "https://dl.example.com/widget_1.2.3_amd64.deb" {
		t.Fatalf("redirect missing or wrong: %+v (ok=%v)", rd, ok)
	}
	if _, ok := snap.Files["/dists/stable/InRelease"]; !ok {
		t.Fatal("InRelease missing")
	}
	pk := string(snap.Files["/dists/stable/main/binary-amd64/Packages"].Data)
	if !strings.Contains(pk, "Package: widget") || !strings.Contains(pk, "Package: acme-archive-keyring") {
		t.Fatalf("Packages missing entries:\n%s", pk)
	}
}
