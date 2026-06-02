package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jetbrainsLikeUpdates mirrors the shape of www.jetbrains.com/updates/updates.xml:
// the <build> element under <channel id="..."> carries a `number` attribute
// (the version string) plus a `fullNumber` attribute. We pluck the version
// via XPath and use it to render a download URL.
const jetbrainsLikeUpdates = `<?xml version="1.0" encoding="UTF-8"?>
<products>
  <product name="IntelliJ IDEA">
    <channel id="IC-IU-RELEASE-licensing-RELEASE" status="release">
      <build number="2024.1.4" fullNumber="241.18034.62" releaseDate="20240620"/>
    </channel>
  </product>
</products>`

func newXMLServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestXMLURL_ColdStartTemplatesAssetURL(t *testing.T) {
	srv := newXMLServer(t, jetbrainsLikeUpdates, http.StatusOK)
	defer srv.Close()

	d, err := NewLatestXMLDiscoverer(srv.URL,
		"//channel[@status='release']/build/@number",
		"https://download.jetbrains.com/idea/ideaIC-{token}.tar.gz",
		srv.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "2024.1.4" {
		t.Errorf("Token = %q, want 2024.1.4", res.Probe.Token)
	}
	if res.Probe.URL != "https://download.jetbrains.com/idea/ideaIC-2024.1.4.tar.gz" {
		t.Errorf("URL = %q, want substituted URL", res.Probe.URL)
	}
	if res.Unchanged {
		t.Errorf("Unchanged = true on cold start")
	}
}

func TestXMLURL_XPathPlaceholdersInTemplate(t *testing.T) {
	body := `<root><v>1.2.3</v><host>dl.example.com</host><a>amd64</a></root>`
	srv := newXMLServer(t, body, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/v",
		"https://{xpath:/root/host}/foo-{token}_{xpath:/root/a}.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.URL != "https://dl.example.com/foo-1.2.3_amd64.deb" {
		t.Errorf("URL = %q", res.Probe.URL)
	}
}

func TestXMLURL_UnchangedWhenTokenMatches(t *testing.T) {
	srv := newXMLServer(t, jetbrainsLikeUpdates, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL,
		"//channel[@status='release']/build/@number",
		"https://x/{token}.tar.gz", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{Token: "2024.1.4"}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Unchanged {
		t.Errorf("Unchanged = false, want true (token matches prev)")
	}
}

func TestXMLURL_TokenXPathMissingIsError(t *testing.T) {
	srv := newXMLServer(t, `<root><x>1</x></root>`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/missing",
		"https://x/{token}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on missing token_xpath")
	}
	if !strings.Contains(err.Error(), "token_xpath") {
		t.Errorf("err = %v, want mention of token_xpath", err)
	}
}

func TestXMLURL_EmptyTokenIsError(t *testing.T) {
	srv := newXMLServer(t, `<root><v></v></root>`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/v",
		"https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on empty token")
	}
}

func TestXMLURL_PlaceholderMissingIsError(t *testing.T) {
	srv := newXMLServer(t, `<root><v>1</v></root>`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/v",
		"https://x/{xpath:/root/not_there}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on missing placeholder")
	}
	if !strings.Contains(err.Error(), "/root/not_there") {
		t.Errorf("err = %v, want mention of missing XPath", err)
	}
}

func TestXMLURL_RejectsNonHTTPRendered(t *testing.T) {
	body := `<root><u>file:///etc/passwd</u><v>1</v></root>`
	srv := newXMLServer(t, body, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/v",
		"{xpath:/root/u}", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on file:// scheme")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Errorf("err = %v, want scheme error", err)
	}
}

func TestXMLURL_NotXMLIsError(t *testing.T) {
	// xmlquery.Parse rejects bytes that don't form a valid XML document.
	// The leading `<` keeps the response from looking like HTML/text fallback
	// — we want the parser to fail on malformed XML specifically.
	srv := newXMLServer(t, `<not really xml at all`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/v",
		"https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on malformed XML body")
	}
}

func TestXMLURL_Non2xxIsError(t *testing.T) {
	srv := newXMLServer(t, "boom", http.StatusServiceUnavailable)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/v",
		"https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on 503")
	}
}

func TestXMLURL_BodyCapEnforced(t *testing.T) {
	big := strings.Repeat("a", xmlURLBodyCap+10)
	body := `<root><v>` + big + `</v></root>`
	srv := newXMLServer(t, body, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestXMLDiscoverer(srv.URL, "/root/v",
		"https://x/{token}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error when body exceeds cap")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want body-cap error", err)
	}
}
