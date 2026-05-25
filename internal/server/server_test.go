package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/refresh"
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
