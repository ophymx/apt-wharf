package index

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

func entry(name, pkg, ver, arch, pool string, size int64, sha string) *Entry {
	control := []byte("Package: " + pkg + "\nVersion: " + ver + "\nArchitecture: " + arch + "\nMaintainer: x\nDescription: y\n")
	fs, err := ExtractFields(control)
	if err != nil {
		panic(err)
	}
	return &Entry{
		SourceName: name,
		Control:    control,
		PoolPath:   pool,
		Size:       size,
		SHA256:     sha,
		Fields:     fs,
	}
}

func TestBuild_TwoArchOneAll(t *testing.T) {
	s := Suite{
		Origin: "Acme", Label: "Acme APT",
		Codename: "stable", Description: "Acme",
		Component:     "main",
		Architectures: []string{"amd64", "arm64"},
	}
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	entries := []*Entry{
		entry("vendor-bar-amd64", "bar", "1.0.0", "amd64", "/pool/main/b/bar/bar_1.0.0_amd64.deb", 100, "sha-amd64"),
		entry("vendor-bar-arm64", "bar", "1.0.0", "arm64", "/pool/main/b/bar/bar_1.0.0_arm64.deb", 110, "sha-arm64"),
		entry("acme-archive-keyring", "acme-archive-keyring", "2026.05.09.1", "all", "/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.05.09.1_all.deb", 1024, "sha-bootstrap"),
	}
	out, err := Build(s, entries, now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, arch := range s.Architectures {
		af, ok := out.PerArch[arch]
		if !ok {
			t.Fatalf("missing arch %s", arch)
		}
		if !bytes.Contains(af.Packages, []byte("Architecture: "+arch)) {
			t.Errorf("arch %s Packages missing arch-specific stanza", arch)
		}
		if !bytes.Contains(af.Packages, []byte("Architecture: all")) {
			t.Errorf("arch %s Packages missing all-arch (bootstrap) stanza", arch)
		}
		// gunzip and compare to plain Packages
		gz, err := gzip.NewReader(bytes.NewReader(af.PackagesGz))
		if err != nil {
			t.Fatalf("gzip read: %v", err)
		}
		decoded, _ := io.ReadAll(gz)
		if !bytes.Equal(decoded, af.Packages) {
			t.Error("Packages.gz body mismatch")
		}
	}

	rel := string(out.ReleaseUnsigned)
	for _, want := range []string{
		"Origin: Acme",
		"Label: Acme APT",
		"Codename: stable",
		"Components: main",
		"Architectures: amd64 arm64",
		"Acquire-By-Hash: yes",
		"main/binary-amd64/Packages",
		"main/binary-amd64/Packages.gz",
		"main/binary-arm64/Packages",
		"main/binary-arm64/Packages.gz",
	} {
		if !strings.Contains(rel, want) {
			t.Errorf("Release missing %q\nfull:\n%s", want, rel)
		}
	}
}

func TestBuild_DuplicateTripleFails(t *testing.T) {
	s := Suite{Codename: "stable", Component: "main", Architectures: []string{"amd64"}}
	entries := []*Entry{
		entry("a", "bar", "1.0", "amd64", "/p/a.deb", 1, "x"),
		entry("b", "bar", "1.0", "amd64", "/p/b.deb", 1, "y"),
	}
	_, err := Build(s, entries, time.Now())
	if err == nil {
		t.Fatal("expected duplicate-triple error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error: %v", err)
	}
	// Both source names must be in the error.
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("error missing source names: %v", err)
	}
}

func TestBuild_UnknownArchFails(t *testing.T) {
	s := Suite{Codename: "stable", Component: "main", Architectures: []string{"amd64"}}
	entries := []*Entry{
		entry("foo-i386", "foo", "1", "i386", "/p/foo.deb", 1, "x"),
	}
	_, err := Build(s, entries, time.Now())
	if err == nil {
		t.Fatal("expected error for arch not in suite")
	}
	if !strings.Contains(err.Error(), "i386") {
		t.Fatalf("error: %v", err)
	}
}

func TestBuild_DeterministicOutput(t *testing.T) {
	s := Suite{Origin: "A", Label: "A", Codename: "stable", Description: "d", Component: "main", Architectures: []string{"amd64"}}
	now := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)

	mk := func(order []string) []*Entry {
		m := map[string]*Entry{
			"foo": entry("foo", "foo", "1.0", "amd64", "/p/foo.deb", 1, "fff"),
			"bar": entry("bar", "bar", "2.0", "amd64", "/p/bar.deb", 2, "bbb"),
			"baz": entry("baz", "baz", "3.0", "amd64", "/p/baz.deb", 3, "ccc"),
		}
		out := make([]*Entry, 0, len(order))
		for _, k := range order {
			out = append(out, m[k])
		}
		return out
	}
	a, _ := Build(s, mk([]string{"foo", "bar", "baz"}), now)
	b, _ := Build(s, mk([]string{"baz", "foo", "bar"}), now)
	if !bytes.Equal(a.PerArch["amd64"].Packages, b.PerArch["amd64"].Packages) {
		t.Error("Packages output is order-dependent")
	}
	if !bytes.Equal(a.ReleaseUnsigned, b.ReleaseUnsigned) {
		t.Error("Release output is order-dependent")
	}
}
