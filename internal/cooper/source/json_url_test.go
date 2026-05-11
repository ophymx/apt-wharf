package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
)

func TestResolveJSONURL_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[{"version":"1.2.3","sha256":"deadbeef"}]}`))
	}))
	defer srv.Close()

	res, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:         srv.URL,
		VersionPath: "releases.0.version",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.2.3" {
		t.Errorf("Version: %q, want 1.2.3", res.Version)
	}
	if res.SourceEpoch <= 0 {
		t.Errorf("SourceEpoch: %d, want > 0", res.SourceEpoch)
	}
}

func TestResolveJSONURL_MissingVersionPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"releases":[]}`))
	}))
	defer srv.Close()

	_, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:         srv.URL,
		VersionPath: "releases.0.version",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestResolveJSONURL_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	_, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:         srv.URL,
		VersionPath: "version",
	})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected invalid-JSON error, got %v", err)
	}
}

func TestResolveJSONURL_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:         srv.URL,
		VersionPath: "version",
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got %v", err)
	}
}

func TestRenderAssetURL_StandardSubstitutions(t *testing.T) {
	got, err := RenderAssetURL(
		"https://example.com/foo-${VERSION}-${ARCH}.tar.gz",
		"1.2.3", "amd64", []byte(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example.com/foo-1.2.3-amd64.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderAssetURL_TokenPlaceholder(t *testing.T) {
	got, err := RenderAssetURL(
		"https://example.com/foo-{token}-linux.tar.gz",
		"1.2.3", "amd64", []byte(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/foo-1.2.3-linux.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestRenderAssetURL_GjsonPlaceholder(t *testing.T) {
	got, err := RenderAssetURL(
		"https://example.com/{releases.0.path}",
		"1.2.3", "amd64",
		[]byte(`{"releases":[{"path":"foo-1.2.3.tar.gz"}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/foo-1.2.3.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestRenderAssetURL_MissingPlaceholder(t *testing.T) {
	_, err := RenderAssetURL(
		"https://example.com/{missing.path}",
		"1.2.3", "amd64", []byte(`{}`),
	)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected missing-placeholder error, got %v", err)
	}
}

func TestRenderAssetURL_RejectsNonHTTPS(t *testing.T) {
	_, err := RenderAssetURL(
		"file:///tmp/${VERSION}.tar.gz",
		"1.2.3", "amd64", []byte(`{}`),
	)
	if err == nil || !strings.Contains(err.Error(), "must be http or https") {
		t.Errorf("expected scheme rejection, got %v", err)
	}
}

func TestDeriveJSONURLEpoch_Stable(t *testing.T) {
	a := deriveJSONURLEpoch("https://x/y", "1.2.3")
	b := deriveJSONURLEpoch("https://x/y", "1.2.3")
	if a != b {
		t.Errorf("not deterministic: %d vs %d", a, b)
	}
	// Different inputs → different epochs (with overwhelming probability).
	c := deriveJSONURLEpoch("https://x/y", "1.2.4")
	if a == c {
		t.Errorf("epoch collision on different version: %d == %d", a, c)
	}
	// Falls within the 2020-01-01 + ~10y window.
	const base = int64(1577836800)
	const span = int64(10 * 365 * 24 * 3600)
	if a < base || a >= base+span {
		t.Errorf("epoch %d outside [%d, %d)", a, base, base+span)
	}
}
