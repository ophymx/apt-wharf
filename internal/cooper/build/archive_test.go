package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is the minimal description of a file/symlink/etc. for the
// in-memory tarballs the tests build.
type tarEntry struct {
	Name     string
	Mode     int64
	Body     string
	Type     byte
	Linkname string
}

func buildTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "in.tar.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Typeflag: typ,
			Linkname: e.Linkname,
		}
		if typ != tar.TypeReg && typ != tar.TypeRegA {
			hdr.Size = 0
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtract_Tgz_HappyPath(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "hugo/bin/hugo", Mode: 0o755, Body: "ELF..."},
		{Name: "hugo/README.md", Mode: 0o644, Body: "# Hugo\n"},
	})
	dest := t.TempDir()
	if err := Extract(src, dest, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"hugo/bin/hugo":  "ELF...",
		"hugo/README.md": "# Hugo\n",
	} {
		got, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Errorf("missing %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("body of %s: %q", rel, got)
		}
	}
	// Mode of executable file preserved (mask + drop suid/sgid leaves perm bits).
	st, err := os.Stat(filepath.Join(dest, "hugo/bin/hugo"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode: %v", st.Mode().Perm())
	}
}

func TestExtract_NoPrefixStripping(t *testing.T) {
	// Design contract: archive's top-level dir becomes a top-level dir
	// under destDir.
	src := buildTarGz(t, []tarEntry{
		{Name: "go-1.21.0/bin/go", Mode: 0o755, Body: "go-binary"},
	})
	dest := t.TempDir()
	if err := Extract(src, dest, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "go-1.21.0/bin/go")); err != nil {
		t.Errorf("expected leading dir preserved: %v", err)
	}
}

func TestExtract_RejectsTraversal(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "../escape.txt", Body: "x"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "traverses up") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestExtract_RejectsAbsolute(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "/etc/passwd-attack", Body: "x"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute rejection, got %v", err)
	}
}

func TestExtract_RejectsSetuid(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "evil", Mode: 0o4755, Body: "exploit"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "setuid/setgid") {
		t.Fatalf("expected setuid rejection, got %v", err)
	}
}

func TestExtract_RejectsDeviceEntries(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "evil-device", Type: tar.TypeChar},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("expected device rejection, got %v", err)
	}
}

func TestExtract_AllowsSymlinkInside(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "bin/python3", Mode: 0o755, Body: "ELF..."},
		{Name: "bin/python", Type: tar.TypeSymlink, Linkname: "python3"},
	})
	dest := t.TempDir()
	if err := Extract(src, dest, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(dest, "bin/python"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected a symlink, got mode %v", st.Mode())
	}
	target, err := os.Readlink(filepath.Join(dest, "bin/python"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "python3" {
		t.Errorf("link target: %s", target)
	}
}

func TestExtract_RejectsSymlinkOutside_Absolute(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "leak", Type: tar.TypeSymlink, Linkname: "/etc/passwd"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("expected escape error, got %v", err)
	}
}

func TestExtract_RejectsSymlinkOutside_Relative(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "leak", Type: tar.TypeSymlink, Linkname: "../../etc/passwd"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("expected escape error, got %v", err)
	}
}

func TestExtract_HardlinkTargetEscape(t *testing.T) {
	src := buildTarGz(t, []tarEntry{
		{Name: "evil-hardlink", Type: tar.TypeLink, Linkname: "../../etc/passwd"},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "traverses up") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestExtract_MaxFilesEnforced(t *testing.T) {
	entries := make([]tarEntry, 5)
	for i := range entries {
		entries[i] = tarEntry{Name: filepath.Join("d", "f"+itoa(i)), Body: "x"}
	}
	src := buildTarGz(t, entries)
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{MaxFiles: 3})
	if err == nil || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Fatalf("expected file-count cap, got %v", err)
	}
}

func TestExtract_MaxBytesEnforced(t *testing.T) {
	body := strings.Repeat("a", 100)
	src := buildTarGz(t, []tarEntry{
		{Name: "big", Body: body},
		{Name: "bigger", Body: body},
	})
	dest := t.TempDir()
	err := Extract(src, dest, ExtractOpts{MaxBytes: 50})
	if err == nil || !strings.Contains(err.Error(), "uncompressed bytes") {
		t.Fatalf("expected byte cap, got %v", err)
	}
}

func TestExtract_NotAnArchive(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "raw-binary")
	_ = os.WriteFile(tmp, []byte("not an archive"), 0o644)
	err := Extract(tmp, t.TempDir(), ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "not an archive") {
		t.Fatalf("expected not-an-archive error, got %v", err)
	}
}

func TestIsArchive(t *testing.T) {
	cases := map[string]bool{
		"hugo.tar.gz":  true,
		"hugo.tgz":     true,
		"hugo.tar.xz":  true,
		"hugo.txz":     true,
		"hugo.tar.zst": true,
		"hugo.tar.bz2": true,
		"hugo.zip":     true,
		"hugo.deb":     false,
		"hugo":         false,
		"hugo.tar":     false, // we don't unpack uncompressed tar — design lists only the compressed forms
	}
	for name, want := range cases {
		if got := IsArchive(name); got != want {
			t.Errorf("IsArchive(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestExtract_Zip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "in.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("kubelogin/kubelogin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ELF...")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := Extract(p, dest, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "kubelogin/kubelogin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ELF..." {
		t.Errorf("body: %q", got)
	}
}

func TestExtract_Zip_TraversalRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evil.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("../escape.txt"); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Extract(p, t.TempDir(), ExtractOpts{})
	if err == nil || !strings.Contains(err.Error(), "traverses up") {
		t.Fatalf("expected zip traversal rejection, got %v", err)
	}
}

// itoa is a tiny stdlib-free integer-to-string for the file-cap test
// fixture; testing.T doesn't expose strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// buildTarGzWithPAXGlobal mirrors buildTarGz but prepends a single PAX
// global header carrying a "comment" record — the exact shape git
// archive emits at the top of every github.com/<repo>/archive/refs/
// tags/*.tar.gz. tar.Writer is picky about TypeXGlobalHeader (only
// PAXRecords + Format may be set), so this is a separate helper rather
// than a tarEntry extension.
func buildTarGzWithPAXGlobal(t *testing.T, entries []tarEntry) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "in.tar.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"},
	}); err != nil {
		t.Fatalf("write PAX global: %v", err)
	}

	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Typeflag: typ,
			Linkname: e.Linkname,
		}
		if typ != tar.TypeReg && typ != tar.TypeRegA {
			hdr.Size = 0
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return p
}

func TestExtract_SkipsPAXGlobalHeader(t *testing.T) {
	// git archive prepends a PAX global header carrying the commit
	// hash to every github.com/<repo>/archive/refs/tags/*.tar.gz.
	// Cooper used to reject this with "unsupported tar type 103".
	path := buildTarGzWithPAXGlobal(t, []tarEntry{
		{Name: "foo/bar", Body: "hello"},
	})
	dest := t.TempDir()
	if err := Extract(path, dest, ExtractOpts{}); err != nil {
		t.Fatalf("extract should succeed past PAX global header, got %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "foo/bar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Errorf("body: %q, want hello", body)
	}
}

func TestExtract_PAXNotCountedTowardMaxFiles(t *testing.T) {
	// Two real files plus one PAX global. MaxFiles=2 must succeed —
	// PAX headers aren't files and shouldn't burn count budget.
	path := buildTarGzWithPAXGlobal(t, []tarEntry{
		{Name: "a", Body: "a"},
		{Name: "b", Body: "b"},
	})
	dest := t.TempDir()
	if err := Extract(path, dest, ExtractOpts{MaxFiles: 2}); err != nil {
		t.Errorf("PAX globals shouldn't count toward MaxFiles, got %v", err)
	}
}

func TestExtract_BytesAboveCeiling(t *testing.T) {
	// Synthesize a (tiny, valid) archive so we know any failure is the
	// ceiling check rejecting opts, not extraction itself.
	path := buildTarGz(t, []tarEntry{{Name: "foo/bar", Body: "x"}})
	dest := t.TempDir()
	err := Extract(path, dest, ExtractOpts{MaxBytes: 128 << 30}) // 128 GiB > 64 GiB ceiling
	if err == nil || !strings.Contains(err.Error(), "exceeds ceiling") {
		t.Errorf("expected ceiling error, got %v", err)
	}
}

func TestExtract_FilesAboveCeiling(t *testing.T) {
	path := buildTarGz(t, []tarEntry{{Name: "foo/bar", Body: "x"}})
	dest := t.TempDir()
	err := Extract(path, dest, ExtractOpts{MaxFiles: 5_000_000})
	if err == nil || !strings.Contains(err.Error(), "exceeds ceiling") {
		t.Errorf("expected ceiling error, got %v", err)
	}
}

func TestExtract_HigherCapHonored(t *testing.T) {
	// Build a tar that's larger than the default 1 GiB cap would allow
	// in principle, but we test in the small: just confirm a non-default
	// MaxBytes is threaded through (the default-substitution path would
	// reject it as too small if the field were lost).
	path := buildTarGz(t, []tarEntry{{Name: "foo/bar", Body: "hello"}})
	dest := t.TempDir()
	// 4 GiB cap — bigger than default, smaller than ceiling. Must succeed.
	if err := Extract(path, dest, ExtractOpts{MaxBytes: 4 << 30, MaxFiles: 200_000}); err != nil {
		t.Errorf("expected extract to succeed under raised cap, got %v", err)
	}
}
