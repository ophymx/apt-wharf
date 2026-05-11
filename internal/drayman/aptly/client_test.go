package aptly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAptly returns an httptest.Server whose /api/repos/{name}/packages
// endpoint behaves like the real aptly: returns a JSON array of
// records matching the ?q= query against pkgs (matched naively for
// this test fixture). Other endpoints return 404.
func fakeAptly(t *testing.T, pkgs []Package) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/repos/") || !strings.HasSuffix(r.URL.Path, "/packages") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		out := []Package{}
		for _, p := range pkgs {
			if matches(p, q) {
				out = append(out, p)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// matches is a naive query matcher for the test fixture. Supports the
// two shapes drayman emits: hash equality and name+arch.
func matches(p Package, q string) bool {
	if q == "" {
		return true
	}
	if rest, ok := strings.CutPrefix(q, "X-Cooper-Build-Inputs-Hash (= "); ok {
		want := strings.TrimSuffix(rest, ")")
		return p.XCooperBuildInputsHash == want
	}
	// "<name> {<arch>}" form.
	parts := strings.SplitN(q, " {", 2)
	if len(parts) != 2 {
		return false
	}
	name := parts[0]
	arch := strings.TrimSuffix(parts[1], "}")
	return p.Package == name && p.Architecture == arch
}

func TestQueryPackages_FormatDetails(t *testing.T) {
	pkgs := []Package{
		{
			Package: "hugo", Version: "0.140.0", Architecture: "amd64",
			Key:                    "Pamd64 hugo 0.140.0 abc123",
			XCooperBuildInputsHash: "sha256:aaa",
		},
		{
			Package: "hugo", Version: "0.140.0-1", Architecture: "amd64",
			Key:                    "Pamd64 hugo 0.140.0-1 def456",
			XCooperBuildInputsHash: "sha256:bbb",
		},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()

	c := New(srv.URL, nil)
	got, err := c.QueryPackages(context.Background(), "test-repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 packages, got %d", len(got))
	}
	if got[0].Package != "hugo" || got[0].Version != "0.140.0" {
		t.Errorf("first package: %+v", got[0])
	}
	if got[0].XCooperBuildInputsHash != "sha256:aaa" {
		t.Errorf("hash field deserialized wrong: %q", got[0].XCooperBuildInputsHash)
	}
}

func TestHashExists_Found(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k", XCooperBuildInputsHash: "sha256:abc"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.HashExists(context.Background(), "test-repo", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HashExists=true")
	}
}

func TestHashExists_NotFound(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k", XCooperBuildInputsHash: "sha256:abc"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.HashExists(context.Background(), "test-repo", "sha256:deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected HashExists=false")
	}
}

func TestListByNameArch(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k1"},
		{Package: "hugo", Version: "0.140.0-1", Architecture: "amd64", Key: "k2"},
		{Package: "hugo", Version: "0.140.0", Architecture: "arm64", Key: "k3"},
		{Package: "terraform", Version: "1.0.0", Architecture: "amd64", Key: "k4"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.ListByNameArch(context.Background(), "test-repo", "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries (hugo amd64), got %d", len(got))
	}
	wantKeys := map[string]bool{"k1": true, "k2": true}
	for _, p := range got {
		if !wantKeys[p.Key] {
			t.Errorf("unexpected key: %q", p.Key)
		}
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://localhost:8080/", nil)
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL: %q", c.baseURL)
	}
}

func TestQueryPackages_Non2xxBubblesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	_, err := c.QueryPackages(context.Background(), "test-repo", "")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got %v", err)
	}
}
