package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/external"
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

// withMetaServer spins up a fake Zoom metadata endpoint and points the
// package-level metadataURL/httpTimeout at it for the duration of the test.
// Tests must not run in parallel because of the shared package state.
func withMetaServer(t *testing.T, body string, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	origURL, origTimeout := metadataURL, httpTimeout
	metadataURL = srv.URL
	httpTimeout = time.Second
	t.Cleanup(func() {
		metadataURL = origURL
		httpTimeout = origTimeout
		srv.Close()
	})
}

func TestDiscover_ColdStartEmitsURLAndToken(t *testing.T) {
	withMetaServer(t, sampleMetadata, http.StatusOK)

	got, err := discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got.Token != "6.5.7.3298" {
		t.Errorf("Token = %q, want 6.5.7.3298", got.Token)
	}
	if got.URL != "https://zoom.us/client/6.5.7.3298/zoom_amd64.deb" {
		t.Errorf("URL = %q, want substituted production asset URL", got.URL)
	}
	if got.Unchanged {
		t.Errorf("Unchanged = true on cold start")
	}
}

func TestDiscover_UnchangedWhenPrevTokenMatches(t *testing.T) {
	withMetaServer(t, sampleMetadata, http.StatusOK)

	prev := &external.Probe{URL: "...", Token: "6.5.7.3298"}
	got, err := discover(context.Background(), prev)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !got.Unchanged {
		t.Errorf("Unchanged = false, want true (token matches)")
	}
	if got.URL != "" || got.Token != "" {
		t.Errorf("Unchanged response leaked url/token: %+v", got)
	}
}

func TestDiscover_ChangedWhenPrevTokenDiffers(t *testing.T) {
	withMetaServer(t, sampleMetadata, http.StatusOK)

	prev := &external.Probe{URL: "...", Token: "6.5.6.0000"}
	got, err := discover(context.Background(), prev)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got.Unchanged {
		t.Errorf("Unchanged = true, want false (token differs)")
	}
	if got.Token != "6.5.7.3298" {
		t.Errorf("Token = %q, want fresh version", got.Token)
	}
}

func TestDiscover_StatusFalseIsError(t *testing.T) {
	withMetaServer(t, `{"status": false, "errorMessage": "rate limited"}`, http.StatusOK)

	_, err := discover(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error on status=false")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error %q does not mention upstream errorMessage", err)
	}
}

func TestDiscover_Non2xxIsError(t *testing.T) {
	withMetaServer(t, "boom", http.StatusServiceUnavailable)
	if _, err := discover(context.Background(), nil); err == nil {
		t.Fatalf("expected error on 503")
	}
}

func TestDiscover_InvalidVersionRejected(t *testing.T) {
	withMetaServer(t,
		`{"status": true, "result": {"downloadVO": {"zoom": {"version": "../etc/passwd"}}}}`,
		http.StatusOK)
	if _, err := discover(context.Background(), nil); err == nil {
		t.Fatalf("expected error on bogus version")
	}
}

func TestDiscover_MissingVersionRejected(t *testing.T) {
	withMetaServer(t,
		`{"status": true, "result": {"downloadVO": {"zoom": {}}}}`,
		http.StatusOK)
	if _, err := discover(context.Background(), nil); err == nil {
		t.Fatalf("expected error on missing version")
	}
}
