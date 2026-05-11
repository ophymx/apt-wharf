package aptly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAptly returns an httptest.Server whose /api/repos/{name}/packages
// endpoint behaves like the real aptly: returns a JSON array of
// records matching the ?q= query against pkgs (matched naively for
// this test fixture). Other endpoints return 404.
func fakeAptly(t *testing.T, pkgs []Package) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/repos/") || !strings.HasSuffix(r.URL.Path, "/packages") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		out := []Package{}
		for _, p := range pkgs {
			if matches(p, q) {
				out = append(out, p)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// matches is a naive query matcher for the test fixture. Supports the
// two shapes drayman emits: hash equality and name+arch.
func matches(p Package, q string) bool {
	if q == "" {
		return true
	}
	if rest, ok := strings.CutPrefix(q, "X-Cooper-Build-Inputs-Hash (= "); ok {
		want := strings.TrimSuffix(rest, ")")
		return p.XCooperBuildInputsHash == want
	}
	// "<name> {<arch>}" form.
	parts := strings.SplitN(q, " {", 2)
	if len(parts) != 2 {
		return false
	}
	name := parts[0]
	arch := strings.TrimSuffix(parts[1], "}")
	return p.Package == name && p.Architecture == arch
}

func TestQueryPackages_FormatDetails(t *testing.T) {
	pkgs := []Package{
		{
			Package: "hugo", Version: "0.140.0", Architecture: "amd64",
			Key:                    "Pamd64 hugo 0.140.0 abc123",
			XCooperBuildInputsHash: "sha256:aaa",
		},
		{
			Package: "hugo", Version: "0.140.0-1", Architecture: "amd64",
			Key:                    "Pamd64 hugo 0.140.0-1 def456",
			XCooperBuildInputsHash: "sha256:bbb",
		},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()

	c := New(srv.URL, nil)
	got, err := c.QueryPackages(context.Background(), "test-repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 packages, got %d", len(got))
	}
	if got[0].Package != "hugo" || got[0].Version != "0.140.0" {
		t.Errorf("first package: %+v", got[0])
	}
	if got[0].XCooperBuildInputsHash != "sha256:aaa" {
		t.Errorf("hash field deserialized wrong: %q", got[0].XCooperBuildInputsHash)
	}
}

func TestHashExists_Found(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k", XCooperBuildInputsHash: "sha256:abc"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.HashExists(context.Background(), "test-repo", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HashExists=true")
	}
}

func TestHashExists_NotFound(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k", XCooperBuildInputsHash: "sha256:abc"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.HashExists(context.Background(), "test-repo", "sha256:deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected HashExists=false")
	}
}

func TestListByNameArch(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k1"},
		{Package: "hugo", Version: "0.140.0-1", Architecture: "amd64", Key: "k2"},
		{Package: "hugo", Version: "0.140.0", Architecture: "arm64", Key: "k3"},
		{Package: "terraform", Version: "1.0.0", Architecture: "amd64", Key: "k4"},
	}
	srv := fakeAptly(t, pkgs)
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.ListByNameArch(context.Background(), "test-repo", "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries (hugo amd64), got %d", len(got))
	}
	wantKeys := map[string]bool{"k1": true, "k2": true}
	for _, p := range got {
		if !wantKeys[p.Key] {
			t.Errorf("unexpected key: %q", p.Key)
		}
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://localhost:8080/", nil)
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL: %q", c.baseURL)
	}
}

func TestQueryPackages_Non2xxBubblesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	_, err := c.QueryPackages(context.Background(), "test-repo", "")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got %v", err)
	}
}

func TestUploadFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "demo.deb")
	if err := os.WriteFile(src, []byte("fake-deb-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotPath, gotMethod, gotContentType string
	var gotBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		// Read the multipart field "file" so we can assert content.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotBytes, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["incoming/demo.deb"]`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	out, err := c.UploadFile(context.Background(), "incoming", src)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: %s", gotMethod)
	}
	if gotPath != "/api/files/incoming" {
		t.Errorf("path: %s", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("content-type: %s", gotContentType)
	}
	if string(gotBytes) != "fake-deb-bytes" {
		t.Errorf("body: %q", gotBytes)
	}
	if len(out) != 1 || out[0] != "incoming/demo.deb" {
		t.Errorf("out: %v", out)
	}
}

func TestImportFromDir(t *testing.T) {
	var gotPath, gotMethod, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"FailedFiles": [],
			"Report": {
				"Warnings": [],
				"Added": ["hugo_0.161.1_amd64 added"],
				"Removed": []
			}
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	rep, err := c.ImportFromDir(context.Background(), "drayman-demo", "incoming", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: %s", gotMethod)
	}
	if gotPath != "/api/repos/drayman-demo/file/incoming" {
		t.Errorf("path: %s", gotPath)
	}
	if gotQuery != "forceReplace=1" {
		t.Errorf("query: %s", gotQuery)
	}
	if len(rep.Report.Added) != 1 || rep.Report.Added[0] != "hugo_0.161.1_amd64 added" {
		t.Errorf("report: %+v", rep)
	}
}

func TestImportFromDir_NoForceReplace(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"FailedFiles":[],"Report":{"Warnings":[],"Added":[],"Removed":[]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	_, err := c.ImportFromDir(context.Background(), "r", "d", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "" {
		t.Errorf("expected empty query when forceReplace=false, got %q", gotQuery)
	}
}

func TestPublishUpdate(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Prefix":".", "Distribution":"stable"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if err := c.PublishUpdate(context.Background(), ".", "stable", PublishUpdateOpts{}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: %s", gotMethod)
	}
	// Aptly's URL convention: "." is encoded as ":." so the bare "."
	// isn't path-collapsed.
	if gotPath != "/api/publish/:./stable" {
		t.Errorf("path: %s", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type: %s", gotContentType)
	}
}

func TestPublishUpdate_SkipSigning(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if err := c.PublishUpdate(context.Background(), ".", "stable", PublishUpdateOpts{SkipSigning: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"Signing":{"Skip":true}`) {
		t.Errorf("body missing Signing.Skip: %s", gotBody)
	}
}

func TestPublishUpdate_NamedPrefix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if err := c.PublishUpdate(context.Background(), "internal", "stable", PublishUpdateOpts{}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/publish/internal/stable" {
		t.Errorf("path: %s", gotPath)
	}
}

func TestPublishUpdate_Non2xxBubblesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publication not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	err := c.PublishUpdate(context.Background(), ".", "missing", PublishUpdateOpts{})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got %v", err)
	}
}
