package reprepro

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// stubTransport returns canned content per path. Tests construct one
// per scenario.
type stubTransport struct {
	files      map[string]string // path → body (omitting → fs.ErrNotExist)
	listResult []string          // paths returned by listPackagesFiles
	listErr    error
	cmds       []recordedCmd     // log of every runCmd
	cmdResp    map[string][]byte // name+" "+args[0] → stdout body for runCmd
	cmdErr     error
	staged     []string // paths returned by stageFile
	removed    []string // paths passed to removeFile
}

type recordedCmd struct {
	name string
	args []string
}

func (s *stubTransport) runCmd(_ context.Context, name string, args ...string) ([]byte, error) {
	s.cmds = append(s.cmds, recordedCmd{name: name, args: args})
	if s.cmdErr != nil {
		return nil, s.cmdErr
	}
	key := name
	if len(args) > 0 {
		key += " " + args[0]
	}
	return s.cmdResp[key], nil
}

func (s *stubTransport) readFile(_ context.Context, path string) ([]byte, error) {
	body, ok := s.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(body), nil
}

func (s *stubTransport) listPackagesFiles(_ context.Context, _ string) ([]string, error) {
	return s.listResult, s.listErr
}

func (s *stubTransport) stageFile(_ context.Context, localPath string) (string, error) {
	staged := "/tmp/staged-" + localPath
	s.staged = append(s.staged, staged)
	return staged, nil
}

func (s *stubTransport) removeFile(_ context.Context, path string) error {
	s.removed = append(s.removed, path)
	return nil
}

func TestBackend_HashExists_Found(t *testing.T) {
	st := &stubTransport{
		listResult: []string{"/base/dists/stable/main/binary-amd64/Packages"},
		files: map[string]string{
			"/base/dists/stable/main/binary-amd64/Packages": `Package: hugo
Version: 0.140.0
Architecture: amd64
X-Cooper-Build-Inputs-Hash: sha256:abc
`,
		},
	}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	got, err := b.HashExists(context.Background(), "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("HashExists=false, want true")
	}
}

func TestBackend_HashExists_NotFound(t *testing.T) {
	st := &stubTransport{
		listResult: []string{"/base/dists/stable/main/binary-amd64/Packages"},
		files: map[string]string{
			"/base/dists/stable/main/binary-amd64/Packages": "Package: hugo\nVersion: 0.140.0\nArchitecture: amd64\nX-Cooper-Build-Inputs-Hash: sha256:other\n",
		},
	}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	got, err := b.HashExists(context.Background(), "sha256:missing")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("HashExists=true, want false")
	}
}

func TestBackend_HashExists_EmptyRepo(t *testing.T) {
	// listPackagesFiles returns nothing — repo has no published
	// architectures yet. Should not error.
	st := &stubTransport{}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	got, err := b.HashExists(context.Background(), "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("HashExists on empty repo returned true")
	}
}

func TestBackend_ListByNameArch(t *testing.T) {
	st := &stubTransport{
		listResult: []string{
			"/base/dists/stable/main/binary-amd64/Packages",
			"/base/dists/stable/main/binary-arm64/Packages",
		},
		files: map[string]string{
			"/base/dists/stable/main/binary-amd64/Packages": `Package: hugo
Version: 0.140.0
Architecture: amd64

Package: hugo
Version: 0.140.0-1
Architecture: amd64
`,
			"/base/dists/stable/main/binary-arm64/Packages": `Package: hugo
Version: 0.140.0
Architecture: arm64
`,
		},
	}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	got, err := b.ListByNameArch(context.Background(), "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (amd64 only)", len(got))
	}
	versions := []string{got[0].Version, got[1].Version}
	if !contains(versions, "0.140.0") || !contains(versions, "0.140.0-1") {
		t.Errorf("versions: %v", versions)
	}
}

func TestBackend_Import_RunsRepreproIncludedeb(t *testing.T) {
	st := &stubTransport{}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	if err := b.Import(context.Background(), "/tmp/hugo_0.140.0_amd64.deb"); err != nil {
		t.Fatal(err)
	}
	if len(st.cmds) != 1 {
		t.Fatalf("want 1 cmd, got %d", len(st.cmds))
	}
	c := st.cmds[0]
	if c.name != "reprepro" {
		t.Errorf("cmd name: %s", c.name)
	}
	wantArgs := []string{"-b", "/base", "includedeb", "stable", "/tmp/staged-/tmp/hugo_0.140.0_amd64.deb"}
	if !equalSlices(c.args, wantArgs) {
		t.Errorf("args: %v, want %v", c.args, wantArgs)
	}
	// Stage + remove should bracket the import.
	if len(st.staged) != 1 || len(st.removed) != 1 {
		t.Errorf("stage/remove counts: staged=%d removed=%d", len(st.staged), len(st.removed))
	}
}

func TestBackend_Import_BubblesUpRepreproError(t *testing.T) {
	st := &stubTransport{cmdErr: errors.New("reprepro: distribution missing")}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	err := b.Import(context.Background(), "/tmp/x.deb")
	if err == nil || !strings.Contains(err.Error(), "distribution missing") {
		t.Errorf("err: %v", err)
	}
	// removeFile should still be called (defer cleanup) even on
	// failure.
	if len(st.removed) != 1 {
		t.Errorf("staged file not cleaned up on error")
	}
}

func TestBackend_Publish_NoOp(t *testing.T) {
	st := &stubTransport{}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}
	if err := b.Publish(context.Background()); err != nil {
		t.Errorf("Publish should be a no-op: %v", err)
	}
	if len(st.cmds) != 0 {
		t.Errorf("Publish ran %d commands; reprepro Publish should be no-op", len(st.cmds))
	}
}

func TestBackend_Import_InvalidatesCache(t *testing.T) {
	st := &stubTransport{
		listResult: []string{"/base/dists/stable/main/binary-amd64/Packages"},
		files: map[string]string{
			"/base/dists/stable/main/binary-amd64/Packages": "Package: hugo\nVersion: 0.140.0\nArchitecture: amd64\nX-Cooper-Build-Inputs-Hash: sha256:old\n",
		},
	}
	b := &Backend{transport: st, basedir: "/base", distribution: "stable", component: "main"}

	// First query populates the cache.
	if exists, _ := b.HashExists(context.Background(), "sha256:old"); !exists {
		t.Fatal("expected old hash to be found before import")
	}

	// Now Import; the cache should be invalidated.
	st.files["/base/dists/stable/main/binary-amd64/Packages"] = "Package: hugo\nVersion: 0.140.0-1\nArchitecture: amd64\nX-Cooper-Build-Inputs-Hash: sha256:new\n"
	if err := b.Import(context.Background(), "/tmp/h.deb"); err != nil {
		t.Fatal(err)
	}
	// Re-query: old hash should no longer be found.
	if exists, _ := b.HashExists(context.Background(), "sha256:old"); exists {
		t.Error("cache was not invalidated after Import — still saw old hash")
	}
	if exists, _ := b.HashExists(context.Background(), "sha256:new"); !exists {
		t.Error("post-import cache miss for new hash")
	}
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
