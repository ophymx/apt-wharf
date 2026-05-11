package reprepro

import "testing"

func TestParsePackages_TwoStanzas(t *testing.T) {
	body := []byte(`Package: hugo
Version: 0.140.0
Architecture: amd64
Maintainer: Demo <demo@example.com>
Description: foo
 (continued line, must be ignored)
X-Cooper-Build-Inputs-Hash: sha256:abc

Package: hugo
Version: 0.140.0-1
Architecture: amd64
X-Cooper-Build-Inputs-Hash: sha256:def
`)
	got := parsePackages(body)
	if len(got) != 2 {
		t.Fatalf("got %d stanzas, want 2", len(got))
	}
	if got[0].Name != "hugo" || got[0].Version != "0.140.0" || got[0].Architecture != "amd64" {
		t.Errorf("stanza 0: %+v", got[0])
	}
	if got[0].BuildInputsHash != "sha256:abc" {
		t.Errorf("stanza 0 hash: %q", got[0].BuildInputsHash)
	}
	if got[1].Version != "0.140.0-1" || got[1].BuildInputsHash != "sha256:def" {
		t.Errorf("stanza 1: %+v", got[1])
	}
}

func TestParsePackages_Empty(t *testing.T) {
	if got := parsePackages(nil); len(got) != 0 {
		t.Errorf("empty input → %d records", len(got))
	}
}

func TestParsePackages_TrailingNoBlank(t *testing.T) {
	body := []byte("Package: foo\nVersion: 1.0\nArchitecture: amd64\n")
	got := parsePackages(body)
	if len(got) != 1 || got[0].Name != "foo" {
		t.Errorf("trailing stanza missed: %+v", got)
	}
}

func TestParsePackages_SkipsStanzaWithoutPackageField(t *testing.T) {
	body := []byte(`Description: orphan field
Version: 1.0

Package: real
Version: 2.0
Architecture: amd64
`)
	got := parsePackages(body)
	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("orphan-stanza filter wrong: %+v", got)
	}
}

func TestParsePackages_PreservesPackageMinusArch(t *testing.T) {
	// Common in practice: multiple stanzas for the same Package
	// across architectures.
	body := []byte(`Package: hugo
Version: 0.140.0
Architecture: amd64
X-Cooper-Build-Inputs-Hash: sha256:a64

Package: hugo
Version: 0.140.0
Architecture: arm64
X-Cooper-Build-Inputs-Hash: sha256:r64
`)
	got := parsePackages(body)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Architecture != "amd64" || got[1].Architecture != "arm64" {
		t.Errorf("arches: %s, %s", got[0].Architecture, got[1].Architecture)
	}
}
