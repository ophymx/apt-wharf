package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloader_FetchHashesAndSizes(t *testing.T) {
	body := []byte("hello cooper\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset")
	d := NewDownloader(srv.Client())
	hex, n, err := d.Fetch(context.Background(), srv.URL, dst, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Errorf("size: got %d", n)
	}
	const want = "3970692c3f3fdfb043ff0ae38ba216e9ba579fc6ee434124554b38e47ae08550"
	if hex != want {
		t.Errorf("hash drifted:\n got  %s\n want %s", hex, want)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(body) {
		t.Errorf("body roundtrip: %q", got)
	}
}

func TestDownloader_SizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	d := NewDownloader(srv.Client())
	dst := filepath.Join(t.TempDir(), "x")
	_, _, err := d.Fetch(context.Background(), srv.URL, dst, 9999)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected size mismatch, got %v", err)
	}
}

func TestDownloader_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	d := NewDownloader(srv.Client())
	_, _, err := d.Fetch(context.Background(), srv.URL, filepath.Join(t.TempDir(), "x"), 0)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404, got %v", err)
	}
}

func TestVerifySHA256(t *testing.T) {
	hex := "abcdef"
	want := "sha256:abcdef"
	if err := VerifySHA256(&want, hex); err != nil {
		t.Errorf("happy: %v", err)
	}

	bad := "sha256:zzzz"
	if err := VerifySHA256(&bad, hex); err == nil {
		t.Error("expected mismatch error")
	}

	noPrefix := "abcdef"
	if err := VerifySHA256(&noPrefix, hex); err == nil {
		t.Error("expected prefix error")
	}

	if err := VerifySHA256(nil, hex); err != nil {
		t.Errorf("nil expected should be ok: %v", err)
	}
	empty := ""
	if err := VerifySHA256(&empty, hex); err != nil {
		t.Errorf("empty expected should be ok: %v", err)
	}
}
