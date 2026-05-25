package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/refresh"
	"github.com/ophymx/apt-wharf/internal/status"
	"github.com/ophymx/apt-wharf/internal/store"
)

func newPopulatedTracker(t *testing.T) *status.Tracker {
	t.Helper()
	tr := status.NewTracker()
	tr.Seed(map[string]*store.SourceState{
		"vendor-foo": {
			Name:        "vendor-foo",
			AssetURL:    "https://x/foo.deb",
			AssetSize:   1234,
			AssetSHA256: "abcd",
			ReleaseTag:  "v1.2.3",
			Control:     "Package: foo\nVersion: 1.2.3\nArchitecture: amd64\n",
			LastChecked: time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC),
			LastChanged: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		},
	},
		map[string]bool{"vendor-foo": true, "vendor-bar": true},
		map[string]string{"vendor-foo": "github_release", "vendor-bar": "external"})
	tr.SeedBootstrap(&store.BootstrapState{
		Version:   "2026.05.09.0",
		Filename:  "foo-archive-keyring_2026.05.09.0_all.deb",
		Size:      4096,
		SHA256:    "deadbeef",
		InputHash: "abc",
	})
	tr.RecordTickStart(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	tr.RecordTickEnd(750*time.Millisecond, 8, 3, nil)
	tr.RecordSourceError("vendor-bar", errors.New("upstream timeout"),
		time.Date(2026, 5, 9, 12, 0, 1, 0, time.UTC))
	return tr
}

func newPopulatedSnapshot() *refresh.Holder {
	h := &refresh.Holder{}
	h.Store(&refresh.Snapshot{
		Files:     map[string]refresh.FileEntry{"/dists/stable/Release": {Data: []byte("x")}},
		Redirects: map[string]refresh.Redirect{"/pool/main/x.deb": {URL: "https://up/x.deb"}},
		BuiltAt:   time.Date(2026, 5, 9, 12, 0, 1, 0, time.UTC),
	})
	return h
}

func TestStatusEndpoint_JSONShape(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), newPopulatedTracker(t), nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}

	tick := got["tick"].(map[string]any)
	if tick["result"] != "ok" {
		t.Errorf("tick.result = %v, want ok", tick["result"])
	}
	if tick["ok_count"].(float64) != 1 {
		t.Errorf("tick.ok_count = %v, want 1", tick["ok_count"])
	}

	snap := got["snapshot"].(map[string]any)
	if snap["files"].(float64) != 1 || snap["redirects"].(float64) != 1 {
		t.Errorf("snapshot counts wrong: %+v", snap)
	}

	srcs := got["sources"].(map[string]any)
	foo := srcs["vendor-foo"].(map[string]any)
	if foo["version"] != "1.2.3" || foo["architecture"] != "amd64" {
		t.Errorf("vendor-foo content fields wrong: %+v", foo)
	}
	bar := srcs["vendor-bar"].(map[string]any)
	if bar["last_result"] != "error" || bar["last_error"] != "upstream timeout" {
		t.Errorf("vendor-bar error fields wrong: %+v", bar)
	}
}

func TestStatusEndpoint_HEAD(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), newPopulatedTracker(t), nil))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/status", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("HEAD /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned body %q", body)
	}
}

func TestStatusEndpoint_RejectsPost(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), newPopulatedTracker(t), nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/status", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestMetricsEndpoint_PrometheusShape(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), newPopulatedTracker(t), nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		"# HELP signpost_refresh_tick_total ",
		"# TYPE signpost_refresh_tick_total counter",
		`signpost_refresh_tick_total{result="ok"} 1`,
		`signpost_refresh_tick_total{result="error"} 0`,
		`signpost_source_status{source="vendor-foo"} NaN`,
		`signpost_source_status{source="vendor-bar"} 0`,
		`signpost_source_asset_size_bytes{source="vendor-foo"} 1234`,
		`signpost_bootstrap_version_info{version="2026.05.09.0"} 1`,
		"signpost_bootstrap_size_bytes 4096",
		"signpost_snapshot_files 1",
		"signpost_snapshot_redirects 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing line %q in:\n%s", want, text)
		}
	}
}

func TestMetricsEndpoint_NilTrackerEmitsZeros(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with nil tracker", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `signpost_refresh_tick_total{result="ok"} 0`) {
		t.Errorf("nil tracker metrics body missing zero counters:\n%s", body)
	}
}

func TestStatusEndpoint_NilTrackerEmptySources(t *testing.T) {
	srv := httptest.NewServer(Handler(newPopulatedSnapshot(), nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &got)
	srcs := got["sources"].(map[string]any)
	if len(srcs) != 0 {
		t.Errorf("nil tracker should yield empty sources map, got %d entries", len(srcs))
	}
}
