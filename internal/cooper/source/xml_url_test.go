package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
)

const jetbrainsLikeUpdates = `<?xml version="1.0" encoding="UTF-8"?>
<products>
  <product name="IntelliJ IDEA">
    <channel id="IC-IU-RELEASE-licensing-RELEASE" status="release">
      <build number="2024.1.4" fullNumber="241.18034.62"/>
    </channel>
  </product>
</products>`

func TestResolveXMLURL_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(jetbrainsLikeUpdates))
	}))
	defer srv.Close()

	res, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:          srv.URL,
		VersionXPath: "//channel[@status='release']/build/@number",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "2024.1.4" {
		t.Errorf("Version: %q, want 2024.1.4", res.Version)
	}
	if res.SourceEpoch <= 0 {
		t.Errorf("SourceEpoch: %d, want > 0", res.SourceEpoch)
	}
}

func TestResolveXMLURL_StripPrefix(t *testing.T) {
	// Some vendor feeds prefix versions ("v2024.1.4"); strip-prefix
	// drops it so the result is a valid Debian upstream version.
	body := `<root><v>v2024.1.4</v></root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:                srv.URL,
		VersionXPath:       "/root/v",
		VersionStripPrefix: "v",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "2024.1.4" {
		t.Errorf("Version: %q, want 2024.1.4", res.Version)
	}
}

func TestResolveXMLURL_StripPrefix_NotFound(t *testing.T) {
	body := `<root><v>2024.1.4</v></root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	_, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:                srv.URL,
		VersionXPath:       "/root/v",
		VersionStripPrefix: "v",
	})
	if err == nil || !strings.Contains(err.Error(), "not found at start") {
		t.Errorf("expected strip-prefix-not-found error, got %v", err)
	}
}

func TestResolveXMLURL_MissingVersionXPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<root><x>1</x></root>`))
	}))
	defer srv.Close()

	_, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:          srv.URL,
		VersionXPath: "/root/missing",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestResolveXMLURL_InvalidXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<unclosed-tag`))
	}))
	defer srv.Close()

	_, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:          srv.URL,
		VersionXPath: "/v",
	})
	if err == nil || !strings.Contains(err.Error(), "parse XML") {
		t.Errorf("expected XML-parse error, got %v", err)
	}
}

func TestResolveXMLURL_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := ResolveXMLURL(context.Background(), nil, &config.XMLURLSource{
		URL:          srv.URL,
		VersionXPath: "/v",
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got %v", err)
	}
}

func TestRenderXMLAssetURL_StandardSubstitutions(t *testing.T) {
	got, err := RenderXMLAssetURL(
		"https://example.com/foo-${VERSION}-${ARCH}.tar.gz",
		"1.2.3", "amd64", []byte(`<root/>`),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example.com/foo-1.2.3-amd64.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderXMLAssetURL_TokenPlaceholder(t *testing.T) {
	got, err := RenderXMLAssetURL(
		"https://example.com/foo-{token}-linux.tar.gz",
		"1.2.3", "amd64", []byte(`<root/>`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/foo-1.2.3-linux.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestRenderXMLAssetURL_XPathPlaceholder(t *testing.T) {
	body := []byte(`<r><build path="foo-1.2.3.tar.gz"/></r>`)
	got, err := RenderXMLAssetURL(
		"https://example.com/{xpath:/r/build/@path}",
		"1.2.3", "amd64", body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/foo-1.2.3.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestRenderXMLAssetURL_MissingPlaceholder(t *testing.T) {
	_, err := RenderXMLAssetURL(
		"https://example.com/{xpath:/missing/path}",
		"1.2.3", "amd64", []byte(`<root/>`),
	)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected missing-placeholder error, got %v", err)
	}
}

func TestRenderXMLAssetURL_RejectsNonHTTPS(t *testing.T) {
	_, err := RenderXMLAssetURL(
		"file:///tmp/${VERSION}.tar.gz",
		"1.2.3", "amd64", []byte(`<root/>`),
	)
	if err == nil || !strings.Contains(err.Error(), "must be http or https") {
		t.Errorf("expected scheme rejection, got %v", err)
	}
}
