package bootstrap

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNextVersion(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct{ prev, want string }{
		{"", "2026.05.09.1"},
		{"2026.05.09.1", "2026.05.09.2"},
		{"2026.05.09.7", "2026.05.09.8"},
		{"2026.05.08.5", "2026.05.09.1"},      // different day
		{"garbage", "2026.05.09.1"},           // unparseable
		{"2026.05.09.0", "2026.05.09.1"},      // n=0 invalid → reset
		{"2026.05.09.x", "2026.05.09.1"},      // n not int → reset
	}
	for _, c := range cases {
		got := NextVersion(c.prev, now)
		if got != c.want {
			t.Errorf("NextVersion(%q) = %q, want %q", c.prev, got, c.want)
		}
	}
}

func TestInputHash_StableUnderEqualInputs(t *testing.T) {
	a := &Inputs{
		PackageName:   "acme-archive-keyring",
		Maintainer:    "Acme <ops@acme>",
		Description:   "Acme APT keyring",
		BaseURL:       "https://apt.acme.example",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64", "arm64"},
		KeyringBytes:  []byte("dummy-keyring-bytes"),
	}
	b := *a
	b.Components = []string{"main"}                // same content, fresh slice
	b.Architectures = []string{"arm64", "amd64"}   // unsorted; hash should sort

	if InputHash(a) != InputHash(&b) {
		t.Fatal("hash should be insensitive to architectures order")
	}
}

func TestInputHash_FlipsOnAnyChange(t *testing.T) {
	base := &Inputs{
		PackageName:   "acme-archive-keyring",
		Maintainer:    "Acme",
		Description:   "x",
		BaseURL:       "https://apt.acme.example",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64"},
		KeyringBytes:  []byte("k1"),
	}
	h0 := InputHash(base)

	mutators := []func(*Inputs){
		func(i *Inputs) { i.PackageName = "different" },
		func(i *Inputs) { i.BaseURL = "https://other.example" },
		func(i *Inputs) { i.Codename = "testing" },
		func(i *Inputs) { i.Components = []string{"main", "extra"} },
		func(i *Inputs) { i.Architectures = []string{"amd64", "arm64"} },
		func(i *Inputs) { i.KeyringBytes = []byte("k2") },
	}
	for _, m := range mutators {
		mut := *base
		m(&mut)
		if InputHash(&mut) == h0 {
			t.Errorf("hash didn't change for mutation: before=%+v after=%+v", base, mut)
		}
	}
}

func TestPoolPath(t *testing.T) {
	got := PoolPath("acme-archive-keyring", "2026.05.09.1")
	want := "/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.05.09.1_all.deb"
	if got != want {
		t.Fatalf("PoolPath = %s, want %s", got, want)
	}
}

func TestBuild_ProducesValidDeb(t *testing.T) {
	in := &Inputs{
		PackageName:   "acme-archive-keyring",
		Maintainer:    "Acme Ops <ops@acme.example>",
		Description:   "Acme APT keyring",
		BaseURL:       "https://apt.acme.example",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64", "arm64"},
		KeyringBytes:  []byte("not-a-real-keyring-but-bytes-suffice-for-build-test"),
	}
	body, sha, err := Build(in, "2026.05.09.1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("!<arch>\n")) {
		t.Fatal("body does not start with ar magic")
	}
	if sha == "" {
		t.Fatal("empty sha256")
	}
	if len(body) < 256 {
		t.Fatalf("suspiciously small .deb (%d bytes)", len(body))
	}
}

func TestRenderDeb822(t *testing.T) {
	in := &Inputs{
		PackageName:   "acme-archive-keyring",
		BaseURL:       "https://apt.acme.example",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64", "arm64"},
	}
	got := string(renderDeb822(in))
	for _, want := range []string{
		"Types: deb",
		"URIs: https://apt.acme.example",
		"Suites: stable",
		"Components: main",
		"Architectures: amd64 arm64",
		"Signed-By: /usr/share/keyrings/acme-archive-keyring.gpg",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deb822 missing %q\nfull:\n%s", want, got)
		}
	}
}
