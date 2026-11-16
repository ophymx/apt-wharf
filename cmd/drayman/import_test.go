package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/drayman/audit"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/internal/drayman/policy"
)

// prebuilt names a .deb the way a vendor repack script would: no
// hash, conventional file name. The file itself is never opened by
// runImport — the stub backend only records the path.
func prebuilt(name, version string) policy.Deb {
	return policy.Deb{
		Path:    fmt.Sprintf("/out/%s_%s_amd64.deb", name, version),
		Name:    name,
		Version: version,
		Arch:    "amd64",
	}
}

// listed is a hashless repo entry, as an earlier import left it.
func listed(name, version string) backend.Package {
	return backend.Package{Name: name, Version: version, Architecture: "amd64", Ref: "ref-" + name + "-" + version}
}

func importOptsFor(be backend.Backend, stdout, stderr *bytes.Buffer, debs ...policy.Deb) importOpts {
	return importOpts{Backend: be, BackendKind: "stub", Debs: debs, Stdout: stdout, Stderr: stderr}
}

func TestRunImport_BatchesThenPublishesOnce(t *testing.T) {
	be := newStubBackend()
	be.landOnImport = true
	var stdout, stderr bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr,
		prebuilt("bigapp", "4.2-repack3"), prebuilt("bigapp-data", "4.2-repack3"))

	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	// The pair travels as one backend operation, not two.
	if be.importCalls != 1 || len(be.imports) != 2 {
		t.Fatalf("importCalls=%d imports=%v, want one call carrying both", be.importCalls, be.imports)
	}
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1", be.publishCount)
	}
	out := stdout.String()
	for _, want := range []string{
		"bigapp\tamd64\timport\t4.2-repack3 not in repo; import as built\n",
		"imported: bigapp_4.2-repack3_amd64.deb\n",
		"imported: bigapp-data_4.2-repack3_amd64.deb\n",
		"publish updated\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestRunImport_SkipsVersionsAlreadyInRepo(t *testing.T) {
	be := newStubBackend()
	be.landOnImport = true
	be.listings["bigapp|amd64"] = []backend.Package{listed("bigapp", "4.2-repack3")}
	var stdout, stderr bytes.Buffer

	// Everything present: no upload, no publish, exit 0.
	opts := importOptsFor(be, &stdout, &stderr, prebuilt("bigapp", "4.2-repack3"))
	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if len(be.imports) != 0 || be.publishCount != 0 {
		t.Fatalf("imports=%v publishCount=%d, want none", be.imports, be.publishCount)
	}
	if !strings.Contains(stdout.String(), "bigapp\tamd64\tskip\t4.2-repack3 already in repo\n") ||
		!strings.Contains(stdout.String(), "nothing to import") {
		t.Errorf("stdout: %q", stdout.String())
	}

	// Half present (the retry after a partial landing): only the
	// missing half is uploaded, and the publish follows.
	stdout.Reset()
	opts = importOptsFor(be, &stdout, &stderr,
		prebuilt("bigapp", "4.2-repack3"), prebuilt("bigapp-data", "4.2-repack3"))
	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if strings.Join(be.imports, ",") != "/out/bigapp-data_4.2-repack3_amd64.deb" || be.publishCount != 1 {
		t.Fatalf("imports=%v publishCount=%d, want only bigapp-data and one publish", be.imports, be.publishCount)
	}
}

func TestRunImport_BackendErrorPublishesNothing(t *testing.T) {
	be := newStubBackend()
	be.landOnImport = true
	be.listings["bigapp|amd64"] = []backend.Package{listed("bigapp", "4.0-repack1"), listed("bigapp", "4.1-repack2")}
	be.importErr["bigapp-data_4.2-repack3_amd64.deb"] = errors.New("aptly import failed")
	var stdout, stderr, logBuf bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr,
		prebuilt("bigapp", "4.2-repack3"), prebuilt("bigapp-data", "4.2-repack3"))
	opts.Retain = 1
	opts.AuditLog = audit.NewWriter(&logBuf)

	err := runImport(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "aptly import failed") {
		t.Fatalf("want import error, got %v", err)
	}
	// A pair must not go out half-published, and a failed run must
	// not delete history.
	if be.publishCount != 0 || len(be.removes) != 0 {
		t.Errorf("publishCount=%d removes=%v, want neither", be.publishCount, removedVersions(be))
	}
	if n := strings.Count(logBuf.String(), `"type":"import"`); n != 2 || strings.Count(logBuf.String(), `"result":"error"`) != 2 {
		t.Errorf("both files of a failed batch must be audited as errors:\n%s", logBuf.String())
	}
}

func TestRunImport_AcceptedButNotListedIsFailure(t *testing.T) {
	// reprepro exits 0 when it declines a version it considers
	// superseded. The repo not listing the version is what counts.
	be := newStubBackend()
	be.landOnImport = true
	be.silentlyDropped = map[string]bool{"bigapp_4.1-repack2_amd64.deb": true}
	be.listings["bigapp|amd64"] = []backend.Package{listed("bigapp", "4.2-repack3"), listed("bigapp", "4.3-repack3")}
	var stdout, stderr, logBuf bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr,
		prebuilt("bigapp", "4.1-repack2"), prebuilt("bigapp-data", "4.1-repack2"))
	opts.Retain = 1
	opts.AuditLog = audit.NewWriter(&logBuf)

	if err := runImport(context.Background(), opts); err == nil {
		t.Fatal("expected non-zero result")
	}
	if !strings.Contains(stderr.String(), "import failed: bigapp_4.1-repack2_amd64.deb: the backend accepted it but the repo does not list bigapp 4.1-repack2 amd64") {
		t.Errorf("stderr: %q", stderr.String())
	}
	// The unverified pair is left alone; the one that did land is
	// still published.
	if len(be.removes) != 0 {
		t.Errorf("unsettled pair pruned: %v", removedVersions(be))
	}
	if be.publishCount != 1 || !strings.Contains(stdout.String(), "imported: bigapp-data_4.1-repack2_amd64.deb") {
		t.Errorf("publishCount=%d stdout=%q", be.publishCount, stdout.String())
	}
	if !strings.Contains(logBuf.String(), `"deb_basename":"bigapp_4.1-repack2_amd64.deb","result":"error"`) ||
		!strings.Contains(logBuf.String(), `"deb_basename":"bigapp-data_4.1-repack2_amd64.deb","result":"ok"`) {
		t.Errorf("audit log must record the per-file outcome:\n%s", logBuf.String())
	}
}

// Retain for prebuilt packages: entries without a hash are candidates
// for the pairs the run names — and only those pairs.
func TestRunImport_RetainPrunesHashlessEntriesOfNamedPairs(t *testing.T) {
	be := newStubBackend()
	be.landOnImport = true
	be.listings["bigapp|amd64"] = []backend.Package{
		listed("bigapp", "4.0-repack1"),
		listed("bigapp", "4.1-repack2"),
		listed("bigapp", "4.2-repack2"),
	}
	// Not named by this run: must survive however old it is.
	be.listings["otherapp|amd64"] = []backend.Package{listed("otherapp", "1.0"), listed("otherapp", "2.0"), listed("otherapp", "3.0")}
	var stdout, stderr bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr, prebuilt("bigapp", "4.2-repack3"))
	opts.Retain = 2
	opts.Cleanup = true

	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	// 4.0 is beyond the two newest upstreams (4.2, 4.1); the
	// script bump repack3 supersedes repack2 of the same 4.2.
	if got := strings.Join(removedVersions(be), ","); got != "4.0-repack1,4.2-repack2" {
		t.Fatalf("removed %s, want 4.0-repack1,4.2-repack2", got)
	}
	for _, r := range be.removes {
		if r.Name != "bigapp" {
			t.Errorf("pruned %s %s, a pair this run never named", r.Name, r.Version)
		}
	}
	if strings.Join(be.ops, ",") != "publish,cleanup" {
		t.Errorf("ops=%v, want publish then cleanup", be.ops)
	}
}

func TestRunImport_RetainProtectsTheRunsOwnVersion(t *testing.T) {
	// Importing an older release on purpose: it is beyond retain=1,
	// but the run's own version is never the one removed.
	be := newStubBackend()
	be.landOnImport = true
	be.listings["bigapp|amd64"] = []backend.Package{listed("bigapp", "4.3-repack3")}
	var stdout, stderr bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr, prebuilt("bigapp", "4.1-repack2"))
	opts.Retain = 1

	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if len(be.removes) != 0 {
		t.Errorf("removed %v, want none", removedVersions(be))
	}
}

func TestRunImport_DryRunPredictsWithoutTouching(t *testing.T) {
	be := newStubBackend()
	be.listings["bigapp|amd64"] = []backend.Package{listed("bigapp", "4.0-repack1"), listed("bigapp", "4.1-repack2")}
	var stdout, stderr bytes.Buffer
	opts := importOptsFor(be, &stdout, &stderr, prebuilt("bigapp", "4.2-repack3"))
	opts.Retain = 2
	opts.DryRun = true

	if err := runImport(context.Background(), opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if len(be.imports) != 0 || len(be.removes) != 0 || be.publishCount != 0 {
		t.Fatalf("dry-run touched the backend: imports=%v removes=%v publish=%d", be.imports, be.removes, be.publishCount)
	}
	out := stdout.String()
	if !strings.Contains(out, "bigapp\tamd64\tprune\t4.0-repack1: upstream 4.0 ranks #3 newest; retain=2\n") ||
		!strings.Contains(out, "dry-run: no upload, prune, or publish.") {
		t.Errorf("stdout: %q", out)
	}
}

func TestRunImport_RejectsAmbiguousInputs(t *testing.T) {
	be := newStubBackend()
	var stdout, stderr bytes.Buffer

	a := prebuilt("bigapp", "4.2-repack3")
	b := a
	b.Path = "/elsewhere/rebuilt.deb"
	err := runImport(context.Background(), importOptsFor(be, &stdout, &stderr, a, b))
	if err == nil || !strings.Contains(err.Error(), "are both bigapp 4.2-repack3 amd64") {
		t.Errorf("same triple twice: %v", err)
	}

	c := prebuilt("bigapp-data", "4.2-repack3")
	c.Path = "/elsewhere/" + filepath.Base(a.Path)
	err = runImport(context.Background(), importOptsFor(be, &stdout, &stderr, a, c))
	if err == nil || !strings.Contains(err.Error(), "share the file name") {
		t.Errorf("same basename twice: %v", err)
	}
	if len(be.imports) != 0 {
		t.Errorf("nothing may be uploaded on a rejected batch: %v", be.imports)
	}
}

// writeDeb writes a minimal .deb (ar: debian-binary, control.tar.gz,
// data.tar.gz) with the given control stanza and returns its path.
func writeDeb(t *testing.T, control string) string {
	t.Helper()
	tarGz := func(name string, body []byte) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if name != "" {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&ar, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
		ar.Write(body)
		if len(body)%2 != 0 {
			ar.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tarGz("./control", []byte(control)))
	member("data.tar.gz", tarGz("", nil))

	path := filepath.Join(t.TempDir(), "fixture.deb")
	if err := os.WriteFile(path, ar.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadDeb(t *testing.T) {
	path := writeDeb(t, "Package: bigapp\nVersion: 4.2-repack3\nArchitecture: amd64\nMaintainer: t <t@example.invalid>\nDescription: fixture\n")
	got, err := readDeb(path)
	if err != nil {
		t.Fatalf("readDeb: %v", err)
	}
	want := policy.Deb{Path: path, Name: "bigapp", Version: "4.2-repack3", Arch: "amd64"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadDeb_Rejects(t *testing.T) {
	notDeb := filepath.Join(t.TempDir(), "junk.deb")
	if err := os.WriteFile(notDeb, []byte("not an ar archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDeb(notDeb); err == nil || !strings.Contains(err.Error(), "junk.deb") {
		t.Errorf("non-.deb: %v", err)
	}
	if _, err := readDeb(filepath.Join(t.TempDir(), "absent.deb")); err == nil {
		t.Error("missing file: expected error")
	}
	noVersion := writeDeb(t, "Package: x\nArchitecture: amd64\nDescription: fixture\n")
	if _, err := readDeb(noVersion); err == nil {
		t.Error("control without Version: expected error")
	}
}
