package render

import (
	"strings"
	"testing"
)

func TestStanza_Minimal(t *testing.T) {
	got, err := Stanza(SourceStanza{
		Types:      []string{"deb"},
		URIs:       []string{"https://example.com"},
		Suites:     []string{"bookworm"},
		Components: []string{"main"},
		SignedBy:   "/usr/share/keyrings/example.gpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `Enabled: yes
Types: deb
URIs: https://example.com
Suites: bookworm
Components: main
Signed-By: /usr/share/keyrings/example.gpg
`
	if got != want {
		t.Errorf("stanza mismatch:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func TestStanza_MultiValueFields(t *testing.T) {
	got, err := Stanza(SourceStanza{
		Types:         []string{"deb"},
		URIs:          []string{"https://example.com"},
		Suites:        []string{"bookworm", "trixie"},
		Components:    []string{"main", "contrib"},
		Architectures: []string{"amd64", "arm64"},
		SignedBy:      "/usr/share/keyrings/example.gpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Suites: bookworm trixie") {
		t.Errorf("multi-value Suites should be space-separated; got:\n%s", got)
	}
	if !strings.Contains(got, "Components: main contrib") {
		t.Errorf("multi-value Components should be space-separated; got:\n%s", got)
	}
	if !strings.Contains(got, "Architectures: amd64 arm64") {
		t.Errorf("multi-value Architectures should be space-separated; got:\n%s", got)
	}
}

func TestStanza_FlatRepoOmitsComponents(t *testing.T) {
	got, err := Stanza(SourceStanza{
		Types:         []string{"deb"},
		URIs:          []string{"https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/"},
		Suites:        []string{"/"},
		Components:    nil, // flat repo: no components
		Architectures: []string{"amd64"},
		SignedBy:      "/usr/share/keyrings/nvidia.gpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Components:") {
		t.Errorf("flat repo should not emit Components line; got:\n%s", got)
	}
	if !strings.Contains(got, "Suites: /") {
		t.Errorf("flat repo Suites should be rendered verbatim; got:\n%s", got)
	}
}

func TestStanza_OmitsArchitecturesWhenAbsent(t *testing.T) {
	got, err := Stanza(SourceStanza{
		Types:      []string{"deb"},
		URIs:       []string{"https://example.com"},
		Suites:     []string{"bookworm"},
		Components: []string{"main"},
		SignedBy:   "/usr/share/keyrings/example.gpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Architectures:") {
		t.Errorf("Architectures: should be absent when not specified; got:\n%s", got)
	}
}

func TestStanza_RequiresURIs(t *testing.T) {
	_, err := Stanza(SourceStanza{
		Types:      []string{"deb"},
		Suites:     []string{"bookworm"},
		Components: []string{"main"},
		SignedBy:   "/usr/share/keyrings/example.gpg",
	})
	if err == nil {
		t.Fatal("expected error when URIs is missing")
	}
}

func TestStanza_RequiresSignedBy(t *testing.T) {
	_, err := Stanza(SourceStanza{
		Types:      []string{"deb"},
		URIs:       []string{"https://example.com"},
		Suites:     []string{"bookworm"},
		Components: []string{"main"},
	})
	if err == nil {
		t.Fatal("expected error when SignedBy is missing")
	}
}

func TestPostinst_Stable(t *testing.T) {
	// Postinst must be byte-stable across calls — chandler ships it
	// into aux_files and the bytes feed build_inputs_hash.
	a := Postinst()
	b := Postinst()
	if a != b {
		t.Error("Postinst should return identical bytes across calls")
	}
	if !strings.Contains(a, "apt-get update") {
		t.Errorf("Postinst should call apt-get update; got:\n%s", a)
	}
}
