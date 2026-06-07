package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
)

func TestHandler_FileAndRedirect(t *testing.T) {
	h := &refresh.Holder{}
	h.Store(&refresh.Snapshot{
		Files: map[string]refresh.FileEntry{
			"/dists/stable/InRelease": {Data: []byte("hello"), ContentType: "text/plain"},
		},
		Redirects: map[string]refresh.Redirect{
			"/pool/main/b/bar/bar_1.0.0_amd64.deb": {URL: "https://upstream.example/bar.deb"},
		},
		BuiltAt: time.Now(),
	})

	srv := httptest.NewServer(Handler(h, nil, nil))
	defer srv.Close()

	t.Run("file served as bytes", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/dists/stable/InRelease")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "hello" {
			t.Fatalf("got %d %q", resp.StatusCode, body)
		}
	})

	t.Run("pool path redirects 302", func(t *testing.T) {
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Get(srv.URL + "/pool/main/b/bar/bar_1.0.0_amd64.deb")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "https://upstream.example/bar.deb" {
			t.Fatalf("Location = %q", got)
		}
	})

	t.Run("missing path 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/dne")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestHandler_ConditionalGET(t *testing.T) {
	lm := time.Date(2026, 6, 7, 22, 0, 0, 0, time.UTC)
	httpLM := lm.Format(http.TimeFormat)

	h := &refresh.Holder{}
	h.Store(&refresh.Snapshot{
		Files: map[string]refresh.FileEntry{
			"/dists/stable/InRelease": {
				Data:         []byte("signed-bytes"),
				ContentType:  "text/plain",
				LastModified: lm,
			},
			"/no-validator": {
				Data:        []byte("anon"),
				ContentType: "text/plain",
				// LastModified zero — server must omit Last-Modified header.
			},
		},
		BuiltAt: time.Now(),
	})
	srv := httptest.NewServer(Handler(h, nil, nil))
	defer srv.Close()

	get := func(t *testing.T, path, ims string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if ims != "" {
			req.Header.Set("If-Modified-Since", ims)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	t.Run("If-Modified-Since exact match returns 304 with empty body", func(t *testing.T) {
		resp, body := get(t, "/dists/stable/InRelease", httpLM)
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("status = %d want 304", resp.StatusCode)
		}
		if len(body) != 0 {
			t.Fatalf("body should be empty on 304, got %q", body)
		}
		if got := resp.Header.Get("Last-Modified"); got != httpLM {
			t.Fatalf("Last-Modified = %q want %q", got, httpLM)
		}
	})

	t.Run("If-Modified-Since later than stored returns 304", func(t *testing.T) {
		future := lm.Add(time.Hour).Format(http.TimeFormat)
		resp, _ := get(t, "/dists/stable/InRelease", future)
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("status = %d want 304", resp.StatusCode)
		}
	})

	t.Run("If-Modified-Since older than stored returns 200 with body", func(t *testing.T) {
		old := lm.Add(-time.Hour).Format(http.TimeFormat)
		resp, body := get(t, "/dists/stable/InRelease", old)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d want 200", resp.StatusCode)
		}
		if string(body) != "signed-bytes" {
			t.Fatalf("body = %q", body)
		}
		if got := resp.Header.Get("Last-Modified"); got != httpLM {
			t.Fatalf("Last-Modified = %q want %q", got, httpLM)
		}
	})

	t.Run("no If-Modified-Since returns 200 with Last-Modified header", func(t *testing.T) {
		resp, body := get(t, "/dists/stable/InRelease", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d want 200", resp.StatusCode)
		}
		if string(body) != "signed-bytes" {
			t.Fatalf("body = %q", body)
		}
		if got := resp.Header.Get("Last-Modified"); got != httpLM {
			t.Fatalf("Last-Modified = %q want %q", got, httpLM)
		}
	})

	t.Run("entry with zero LastModified omits header and serves 200", func(t *testing.T) {
		resp, body := get(t, "/no-validator", httpLM)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d want 200", resp.StatusCode)
		}
		if string(body) != "anon" {
			t.Fatalf("body = %q", body)
		}
		if got := resp.Header.Get("Last-Modified"); got != "" {
			t.Fatalf("Last-Modified header should be absent, got %q", got)
		}
	})

	t.Run("malformed If-Modified-Since falls through to 200", func(t *testing.T) {
		resp, body := get(t, "/dists/stable/InRelease", "not a date")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d want 200", resp.StatusCode)
		}
		if string(body) != "signed-bytes" {
			t.Fatalf("body = %q", body)
		}
	})
}

func TestHandler_NoSnapshotYet(t *testing.T) {
	h := &refresh.Holder{}
	srv := httptest.NewServer(Handler(h, nil, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "snapshot") {
		t.Fatalf("body = %q", body)
	}
}
