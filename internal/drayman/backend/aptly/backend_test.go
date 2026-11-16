package aptly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAptlyImport records the write-path requests Backend.Import
// makes. failUpload names a basename whose upload is rejected;
// failedFiles is what the import-from-dir response reports.
type fakeAptlyImport struct {
	failUpload  string
	failedFiles []string
	warnings    []string

	calls []string // "<METHOD> <path>[?query]" in order
}

func (f *fakeAptlyImport) handler(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/files/"):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := r.MultipartForm.File["file"][0].Filename
		if name == f.failUpload {
			http.Error(w, "disk full", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]string{"stage/" + name})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/repos/"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"FailedFiles": f.failedFiles,
			"Report":      map[string]any{"Warnings": f.warnings},
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/files/"):
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func importFixture(t *testing.T, f *fakeAptlyImport) (*Backend, []string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"bigapp_4.2_amd64.deb", "bigapp-data_4.2_amd64.deb"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("deb bytes of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return New(Opts{BaseURL: srv.URL, Repo: "main", Distribution: "stable", StagingDir: "stage"}), paths
}

func TestImport_BatchUploadsAllThenImportsOnce(t *testing.T) {
	f := &fakeAptlyImport{}
	b, paths := importFixture(t, f)
	if err := b.Import(context.Background(), paths...); err != nil {
		t.Fatalf("Import: %v", err)
	}
	want := []string{
		"POST /api/files/stage",
		"POST /api/files/stage",
		"POST /api/repos/main/file/stage?forceReplace=1",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}

	// No paths, no requests.
	f.calls = nil
	if err := b.Import(context.Background()); err != nil || len(f.calls) != 0 {
		t.Errorf("empty Import: err=%v calls=%v", err, f.calls)
	}
}

// aptly only deletes the files an import consumed. Whatever a failed
// batch leaves in the staging dir has to go, or it sits there (at
// multi-GB sizes) and gets swept into the next import.
func TestImport_FailedFilesDropsStagingDir(t *testing.T) {
	f := &fakeAptlyImport{
		failedFiles: []string{"stage/bigapp-data_4.2_amd64.deb"},
		warnings:    []string{"Unable to read file: ar: missing global header"},
	}
	b, paths := importFixture(t, f)
	err := b.Import(context.Background(), paths...)
	if err == nil || !strings.Contains(err.Error(), "bigapp-data_4.2_amd64.deb") ||
		!strings.Contains(err.Error(), "ar: missing global header") {
		t.Fatalf("want error naming the failed file and aptly's reason, got %v", err)
	}
	if last := f.calls[len(f.calls)-1]; last != "DELETE /api/files/stage" {
		t.Errorf("last call %q, want the staging dir dropped", last)
	}
}

func TestImport_UploadErrorDropsStagingDirAndSkipsImport(t *testing.T) {
	f := &fakeAptlyImport{failUpload: "bigapp-data_4.2_amd64.deb"}
	b, paths := importFixture(t, f)
	if err := b.Import(context.Background(), paths...); err == nil {
		t.Fatal("expected upload error")
	}
	want := []string{
		"POST /api/files/stage", // bigapp: staged fine
		"POST /api/files/stage", // bigapp-data: rejected
		"DELETE /api/files/stage",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}
