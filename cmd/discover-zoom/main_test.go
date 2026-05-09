package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sampleMetadata = `{
  "status": true,
  "errorCode": 0,
  "errorMessage": null,
  "result": {
    "downloadVO": {
      "zoom": { "version": "6.5.7.3298" },
      "zoomX86": { "version": "5.4.53391.1108" }
    },
    "os": "linux"
  }
}`

func newMetaServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func decodeOutput(t *testing.T, b []byte) output {
	t.Helper()
	var out output
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode output %q: %v", b, err)
	}
	return out
}

func TestColdStartEmitsURLAndToken(t *testing.T) {
	srv := newMetaServer(t, sampleMetadata, http.StatusOK)
	defer srv.Close()

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decodeOutput(t, stdout.Bytes())
	if got.Token != "6.5.7.3298" {
		t.Errorf("Token = %q, want 6.5.7.3298", got.Token)
	}
	if got.URL != "https://zoom.us/client/6.5.7.3298/zoom_amd64.deb" {
		t.Errorf("URL = %q, want substituted asset URL", got.URL)
	}
	if got.Unchanged {
		t.Errorf("Unchanged = true on cold start")
	}
}

func TestUnchangedWhenPrevTokenMatches(t *testing.T) {
	srv := newMetaServer(t, sampleMetadata, http.StatusOK)
	defer srv.Close()

	stdin := strings.NewReader(`{"prev":{"url":"...","token":"6.5.7.3298"}}`)
	var stdout bytes.Buffer
	if err := run(stdin, &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decodeOutput(t, stdout.Bytes())
	if !got.Unchanged {
		t.Errorf("Unchanged = false, want true (token matches)")
	}
	if got.URL != "" || got.Token != "" {
		t.Errorf("Unchanged response leaked url/token: %+v", got)
	}
}

func TestChangedWhenPrevTokenDiffers(t *testing.T) {
	srv := newMetaServer(t, sampleMetadata, http.StatusOK)
	defer srv.Close()

	stdin := strings.NewReader(`{"prev":{"url":"...","token":"6.5.6.0000"}}`)
	var stdout bytes.Buffer
	if err := run(stdin, &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decodeOutput(t, stdout.Bytes())
	if got.Unchanged {
		t.Errorf("Unchanged = true, want false (token differs)")
	}
	if got.Token != "6.5.7.3298" {
		t.Errorf("Token = %q, want fresh version", got.Token)
	}
}

func TestStatusFalseIsError(t *testing.T) {
	srv := newMetaServer(t,
		`{"status": false, "errorMessage": "rate limited"}`,
		http.StatusOK)
	defer srv.Close()

	var stdout bytes.Buffer
	err := run(strings.NewReader(""), &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second)
	if err == nil {
		t.Fatalf("expected error on status=false")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error %q does not mention upstream errorMessage", err)
	}
}

func TestNon2xxIsError(t *testing.T) {
	srv := newMetaServer(t, "boom", http.StatusServiceUnavailable)
	defer srv.Close()

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err == nil {
		t.Fatalf("expected error on 503")
	}
}

func TestInvalidVersionRejected(t *testing.T) {
	srv := newMetaServer(t,
		`{"status": true, "result": {"downloadVO": {"zoom": {"version": "../etc/passwd"}}}}`,
		http.StatusOK)
	defer srv.Close()

	var stdout bytes.Buffer
	err := run(strings.NewReader(""), &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second)
	if err == nil {
		t.Fatalf("expected error on bogus version")
	}
}

func TestMissingVersionRejected(t *testing.T) {
	srv := newMetaServer(t,
		`{"status": true, "result": {"downloadVO": {"zoom": {}}}}`,
		http.StatusOK)
	defer srv.Close()

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err == nil {
		t.Fatalf("expected error on missing version")
	}
}

func TestAssetURLMustHaveSubstitution(t *testing.T) {
	if err := run(strings.NewReader(""), &bytes.Buffer{}, "http://x", "no-substitution-here", time.Second); err == nil {
		t.Fatalf("expected error when asset URL has no %%s")
	}
}

func TestStdinJSONErrorReported(t *testing.T) {
	srv := newMetaServer(t, sampleMetadata, http.StatusOK)
	defer srv.Close()

	if err := run(strings.NewReader("{not json"), &bytes.Buffer{}, srv.URL,
		"https://zoom.us/client/%s/zoom_amd64.deb", time.Second); err == nil {
		t.Fatalf("expected error on malformed stdin JSON")
	}
}
