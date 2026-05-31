package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
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

func TestResolveJSONURL_StripPrefix(t *testing.T) {
	// go.dev's endpoint returns versions like "go1.26.3"; Debian
	// rejects that since the upstream version must start with a digit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"version":"go1.26.3","stable":true}]`))
	}))
	defer srv.Close()

	res, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:                srv.URL,
		VersionPath:        "0.version",
		VersionStripPrefix: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.26.3" {
		t.Errorf("Version: %q, want 1.26.3", res.Version)
	}
}

func TestResolveJSONURL_StripPrefix_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.26.3"}`))
	}))
	defer srv.Close()
	_, err := ResolveJSONURL(context.Background(), nil, &config.JSONURLSource{
		URL:                srv.URL,
		VersionPath:        "version",
		VersionStripPrefix: "go", // not present
	})
	if err == nil || !strings.Contains(err.Error(), "not found at start") {
		t.Errorf("expected strip-prefix-not-found error, got %v", err)
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

