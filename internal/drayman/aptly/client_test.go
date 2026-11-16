package aptly

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// fakeAptlyDepQuery mimics how a real aptly answers "<name> {<arch>}":
// as a dependency query, which also returns packages that Provide
// <name> and arch:all packages for any concrete <arch>. provides maps
// a package Key to the name it Provides.
func fakeAptlyDepQuery(t *testing.T, pkgs []Package, provides map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(r.URL.Query().Get("q"), " {", 2)
		if len(parts) != 2 {
			http.Error(w, "unsupported query", http.StatusBadRequest)
			return
		}
		name, arch := parts[0], strings.TrimSuffix(parts[1], "}")
		out := []Package{}
		for _, p := range pkgs {
			nameOK := p.Package == name || provides[p.Key] == name
			archOK := p.Architecture == arch || p.Architecture == "all"
			if nameOK && archOK {
				out = append(out, p)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
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

// aptly's dependency-query semantics leak Provides-only and arch:all
// packages into "<name> {<arch>}" results (observed on aptly 1.6.1).
// ListByNameArch must hand back exact (Package, Architecture) matches
// only: anything else gets bumped over or pruned by mistake.
func TestListByNameArch_ExcludesProvidersAndArchAll(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k1"},
		{Package: "hugo-extended", Version: "0.140.0-7", Architecture: "amd64", Key: "k2"},
		{Package: "hugo", Version: "0.139.0", Architecture: "all", Key: "k3"},
		{Package: "hugo", Version: "0.140.0", Architecture: "arm64", Key: "k4"},
	}
	srv := fakeAptlyDepQuery(t, pkgs, map[string]string{"k2": "hugo"})
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.ListByNameArch(context.Background(), "test-repo", "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "k1" {
		t.Fatalf("want only k1 (hugo amd64), got %+v", got)
	}

	got, err = c.ListByNameArch(context.Background(), "test-repo", "hugo", "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "k3" {
		t.Fatalf("want only k3 (hugo all), got %+v", got)
	}
}

func TestListByName_AllArchesExactName(t *testing.T) {
	pkgs := []Package{
		{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k1"},
		{Package: "hugo-extended", Version: "0.140.0", Architecture: "amd64", Key: "k2"},
		{Package: "hugo", Version: "0.140.0", Architecture: "arm64", Key: "k3"},
	}
	// A bare "<name>" is a dependency query too: Provides leak in.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); q != "hugo" {
			http.Error(w, "unexpected query "+q, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(pkgs)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)

	got, err := c.ListByName(context.Background(), "test-repo", "hugo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "k1" || got[1].Key != "k3" {
		t.Fatalf("want k1 and k3 (hugo, any arch), got %+v", got)
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

// A multi-GB .deb must not be buffered: the upload has to stream the
// file with an exact Content-Length, and allocation must not scale
// with file size. The fixture is sparse so the test stays fast.
func TestUploadFile_StreamsLargeFile(t *testing.T) {
	const size = 64 << 20
	src := filepath.Join(t.TempDir(), "big_1.0_amd64.deb")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 1<<20)
	if _, err := rand.Read(head); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(head); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.New()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(wantSum, f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var gotSum []byte
	var gotPartBytes, gotBodyBytes, gotContentLength int64
	var gotChunked bool
	var gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		gotChunked = len(r.TransferEncoding) > 0
		body := &countingReader{r: r.Body}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotFilename = part.FileName()
		h := sha256.New()
		gotPartBytes, _ = io.Copy(h, part)
		gotSum = h.Sum(nil)
		if _, err := mr.NextPart(); err != io.EOF {
			http.Error(w, "expected exactly one part", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, body)
		gotBodyBytes = body.n
		_ = json.NewEncoder(w).Encode([]string{"d/" + gotFilename})
	}))
	defer srv.Close()
	c := New(srv.URL, nil)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := c.UploadFile(context.Background(), "d", src); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	runtime.ReadMemStats(&after)

	if gotFilename != "big_1.0_amd64.deb" {
		t.Errorf("filename: %q", gotFilename)
	}
	if gotPartBytes != size || !bytes.Equal(gotSum, wantSum.Sum(nil)) {
		t.Errorf("server saw %d bytes with a different digest; want %d identical bytes", gotPartBytes, size)
	}
	if gotChunked || gotContentLength != gotBodyBytes {
		t.Errorf("Content-Length=%d chunked=%v, body was %d bytes; want an exact length", gotContentLength, gotChunked, gotBodyBytes)
	}
	// Client and fake server share the process, so this bounds both.
	// A buffered upload would allocate several times the file size.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/4 {
		t.Errorf("allocated %d MiB uploading a %d MiB file; upload must stream", alloc>>20, size>>20)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// The upload is bounded by progress, not total time: a server that
// stops reading must fail the request once the stall window passes.
func TestUploadFile_StallIsCancelled(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big.deb")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	// Large enough that the body can't fit in socket buffers.
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release // never reads the body
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, nil)
	c.UploadStallTimeout = 200 * time.Millisecond

	start := time.Now()
	_, err = c.UploadFile(context.Background(), "d", src)
	if err == nil || !strings.Contains(err.Error(), "made no progress for 200ms") {
		t.Fatalf("want stall error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("stall took %s to surface", elapsed)
	}
}

func TestDeleteUploadDir(t *testing.T) {
	var gotMethod, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)

	if err := c.DeleteUploadDir(context.Background(), "drayman-abc"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/files/drayman-abc" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
	// Already gone is fine; anything else is an error.
	status = http.StatusNotFound
	if err := c.DeleteUploadDir(context.Background(), "drayman-abc"); err != nil {
		t.Errorf("404 should be tolerated: %v", err)
	}
	status = http.StatusInternalServerError
	if err := c.DeleteUploadDir(context.Background(), "drayman-abc"); err == nil {
		t.Error("expected error on 500")
	}
}

func TestDBCleanup(t *testing.T) {
	var gotMethod, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)

	if err := c.DBCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/db/cleanup" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
	status = http.StatusInternalServerError
	if err := c.DBCleanup(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got %v", err)
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

func TestEncodeAptlyPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{".", ":."},
		{"internal", "internal"},
		{"ubuntu/jammy", "ubuntu_jammy"},
		{"foo_bar", "foo__bar"},
		{"a_b/c", "a__b_c"},
	}
	for _, c := range cases {
		if got := encodeAptlyPrefix(c.in); got != c.want {
			t.Errorf("encodeAptlyPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPublishUpdate_404IsErrPublishNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publication not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	err := c.PublishUpdate(context.Background(), ".", "missing", PublishUpdateOpts{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrPublishNotFound) {
		t.Errorf("expected errors.Is(err, ErrPublishNotFound), got %v", err)
	}
}

func TestPublishUpdate_500BubblesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	err := c.PublishUpdate(context.Background(), ".", "stable", PublishUpdateOpts{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error, got %v", err)
	}
	if errors.Is(err, ErrPublishNotFound) {
		t.Errorf("500 should not be classified as ErrPublishNotFound")
	}
}

func TestPublishRepo_PostsCorrectBody(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Distribution":"stable","Prefix":"."}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if err := c.PublishRepo(context.Background(), ".", PublishRepoOpts{
		SourceRepo:   "shakedown",
		Distribution: "stable",
		SkipSigning:  true,
	}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/publish/:." {
		t.Errorf("path = %s, want /api/publish/:.", gotPath)
	}
	// Spot-check the JSON contents — order isn't pinned (Go map iteration)
	// but every expected key/value pair must appear.
	for _, want := range []string{
		`"SourceKind":"local"`,
		`"Sources":[{"Name":"shakedown"}]`,
		`"Distribution":"stable"`,
		`"Signing":{"Skip":true}`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %s; got %s", want, gotBody)
		}
	}
}

func TestRemovePackages_SendsDeleteWithRefs(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Name":"test-repo"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if err := c.RemovePackages(context.Background(), "test-repo", []string{"Pamd64 hugo 0.140.0 abc123"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/repos/test-repo/packages" {
		t.Errorf("request: %s %s", gotMethod, gotPath)
	}
	var body struct{ PackageRefs []string }
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body %q: %v", gotBody, err)
	}
	if len(body.PackageRefs) != 1 || body.PackageRefs[0] != "Pamd64 hugo 0.140.0 abc123" {
		t.Errorf("PackageRefs: %v", body.PackageRefs)
	}
}

func TestRemovePackages_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"no such ref"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	err := c.RemovePackages(context.Background(), "test-repo", []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
}
