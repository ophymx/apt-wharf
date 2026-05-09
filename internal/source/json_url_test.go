package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const zoomLikeMetadata = `{
  "status": true,
  "result": {
    "downloadVO": {
      "zoom": { "version": "6.5.7.3298" }
    }
  }
}`

func newJSONServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestJSONURL_ColdStartTemplatesAssetURL(t *testing.T) {
	srv := newJSONServer(t, zoomLikeMetadata, http.StatusOK)
	defer srv.Close()

	d, err := NewLatestJSONDiscoverer(srv.URL,
		"result.downloadVO.zoom.version",
		"https://zoom.us/client/{token}/zoom_amd64.deb",
		srv.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "6.5.7.3298" {
		t.Errorf("Token = %q, want 6.5.7.3298", res.Probe.Token)
	}
	if res.Probe.URL != "https://zoom.us/client/6.5.7.3298/zoom_amd64.deb" {
		t.Errorf("URL = %q, want substituted asset URL", res.Probe.URL)
	}
	if res.Unchanged {
		t.Errorf("Unchanged = true on cold start")
	}
}

func TestJSONURL_GjsonPathsInTemplate(t *testing.T) {
	body := `{"v":"1.2.3","host":"dl.example.com","arch":"amd64"}`
	srv := newJSONServer(t, body, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v",
		"https://{host}/foo-{v}_{arch}.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.URL != "https://dl.example.com/foo-1.2.3_amd64.deb" {
		t.Errorf("URL = %q", res.Probe.URL)
	}
}

func TestJSONURL_UnchangedWhenTokenMatches(t *testing.T) {
	srv := newJSONServer(t, zoomLikeMetadata, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "result.downloadVO.zoom.version",
		"https://zoom.us/client/{token}/zoom_amd64.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{Token: "6.5.7.3298"}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Unchanged {
		t.Errorf("Unchanged = false, want true (token matches prev)")
	}
}

func TestJSONURL_TokenPathMissingIsError(t *testing.T) {
	srv := newJSONServer(t, `{"foo":"bar"}`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "result.version",
		"https://x/{token}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on missing token_path")
	}
	if !strings.Contains(err.Error(), "token_path") {
		t.Errorf("err = %v, want mention of token_path", err)
	}
}

func TestJSONURL_EmptyTokenIsError(t *testing.T) {
	srv := newJSONServer(t, `{"v":""}`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v", "https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on empty token")
	}
}

func TestJSONURL_PlaceholderMissingIsError(t *testing.T) {
	srv := newJSONServer(t, `{"v":"1"}`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v",
		"https://x/{not.in.json}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on missing placeholder")
	}
	if !strings.Contains(err.Error(), "{not.in.json}") {
		t.Errorf("err = %v, want mention of missing placeholder", err)
	}
}

func TestJSONURL_RejectsNonHTTPRendered(t *testing.T) {
	srv := newJSONServer(t, `{"u":"file:///etc/passwd","v":"1"}`, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v", "{u}", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error on file:// scheme")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Errorf("err = %v, want scheme error", err)
	}
}

func TestJSONURL_NotJSONIsError(t *testing.T) {
	srv := newJSONServer(t, "not json at all", http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v", "https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on non-JSON body")
	}
}

func TestJSONURL_Non2xxIsError(t *testing.T) {
	srv := newJSONServer(t, "boom", http.StatusServiceUnavailable)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v", "https://x/{token}.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatal("expected error on 503")
	}
}

func TestJSONURL_BodyCapEnforced(t *testing.T) {
	// Build a JSON body just over the cap.
	big := strings.Repeat("a", jsonURLBodyCap+10)
	body := `{"v":"` + big + `"}`
	srv := newJSONServer(t, body, http.StatusOK)
	defer srv.Close()

	d, _ := NewLatestJSONDiscoverer(srv.URL, "v", "https://x/{token}.deb", srv.Client())
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatal("expected error when body exceeds cap")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want body-cap error", err)
	}
}
