package refresh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/config"
	"github.com/ophymx/apt-wharf/internal/signpost/fetch"
	"github.com/ophymx/apt-wharf/internal/signpost/sign"
	"github.com/ophymx/apt-wharf/internal/signpost/source"
	"github.com/ophymx/apt-wharf/internal/signpost/status"
	"github.com/ophymx/apt-wharf/internal/signpost/store"
)

// errDiscoverer simulates a misconfigured source whose probe always fails —
// mirrors the field-reported "release vX exposes no asset matching ..." case.
type errDiscoverer struct{ err error }

func (e *errDiscoverer) Probe(ctx context.Context, in source.ProbeInput) (*source.ProbeResult, error) {
	return nil, e.err
}

// okDiscoverer returns a ProbeResult pointing at a static fake asset.
type okDiscoverer struct {
	url   string
	token string
}

func (o *okDiscoverer) Probe(ctx context.Context, in source.ProbeInput) (*source.ProbeResult, error) {
	if in.Prev != nil && in.Prev.Token == o.token {
		return &source.ProbeResult{Unchanged: true}, nil
	}
	return &source.ProbeResult{Probe: source.Probe{URL: o.url, Token: o.token, ReleaseTag: "v1.0.0"}}, nil
}

// TestSyncImportNew_OneSourceFailsOthersContinue locks in the contract change:
// a discover error for a brand-new source no longer crash-loops the daemon.
// The good source is imported, the bad one logs an error and surfaces on the
// tracker, and the composed snapshot serves the good source's metadata.
func TestSyncImportNew_OneSourceFailsOthersContinue(t *testing.T) {
	debBytes := buildFakeDeb(t, "widget", "1.0.0", "amd64")
	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(t, w, r, debBytes)
	}))
	defer assetSrv.Close()

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
			"widget-amd64": {Discovery: config.Discovery{Type: "latest_url", URL: assetSrv.URL + "/widget.deb"}},
			"broken-amd64": {Discovery: config.Discovery{Type: "latest_url", URL: "http://example.invalid/x.deb"}},
		},
	}

	tracker := status.NewTracker()
	rf := New(Options{
		Cfg: cfg, Signer: signer, Store: st,
		Fetcher:    fetch.New(httpClient),
		HTTPClient: httpClient,
		Discoverers: map[string]source.Discoverer{
			"widget-amd64": &okDiscoverer{url: assetSrv.URL + "/widget.deb", token: "v1.0.0"},
			"broken-amd64": &errDiscoverer{err: errors.New(`release v2.22.0 exposes no asset matching ^widget_[0-9.]+_amd64\.deb$`)},
		},
		Tracker: tracker,
		Holder:  &Holder{},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx := context.Background()
	if err := rf.SyncImportNew(ctx); err != nil {
		t.Fatalf("SyncImportNew must not be fatal when a source's probe fails: %v", err)
	}
	if err := rf.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := rf.Snapshot()
	if snap == nil {
		t.Fatal("snapshot not stored")
	}

	pkg, ok := snap.Files["/dists/stable/main/binary-amd64/Packages"]
	if !ok {
		t.Fatal("amd64 Packages missing from snapshot")
	}
	if !strings.Contains(string(pkg.Data), "Package: widget") {
		t.Errorf("good source not in Packages, got:\n%s", pkg.Data)
	}

	st2, err := st.LoadSources()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2["widget-amd64"]; !ok {
		t.Error("good source state not persisted")
	}
	if _, ok := st2["broken-amd64"]; ok {
		t.Error("failed source should not have a state file yet")
	}

	st3 := tracker.Snapshot()
	bs, ok := st3.Sources["broken-amd64"]
	if !ok {
		t.Fatal("tracker should record the failed source")
	}
	if bs.LastResult != "error" {
		t.Errorf("tracker LastResult = %q want %q", bs.LastResult, "error")
	}
	if bs.LastError == "" {
		t.Error("tracker LastError should carry the probe message")
	}
}
