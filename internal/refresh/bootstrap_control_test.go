package refresh

import (
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/bootstrap"
)

// TestBootstrapControl_FromDeb pins down that the bootstrap stanza we
// publish in Packages comes verbatim from the .deb itself. A drift here
// (e.g. missing Installed-Size) was the root cause of apt's phantom
// upgrade loop.
func TestBootstrapControl_FromDeb(t *testing.T) {
	in := &bootstrap.Inputs{
		PackageName:   "ophymx-stitcher-keyring",
		Maintainer:    "Jeffrey T. Peckham <abic@ophymx.com>",
		Description:   "Ophymx Stitcher APT repository signing key and sources list.\n",
		BaseURL:       "http://localhost:8080",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64", "arm64"},
		KeyringBytes:  []byte("dummy-keyring-bytes"),
	}
	body, _, err := bootstrap.Build(in, "2026.05.09.2")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := extractDebControl(body)
	if err != nil {
		t.Fatalf("extractDebControl: %v", err)
	}
	t.Logf("control extracted from .deb:\n%s", got)

	for _, want := range []string{
		"Package: ophymx-stitcher-keyring",
		"Version: 2026.05.09.2",
		"Architecture: all",
		"Maintainer: Jeffrey T. Peckham <abic@ophymx.com>",
		"Section: misc",
		"Priority: optional",
		"Installed-Size:", // <-- the field we previously omitted
		"Description: Ophymx Stitcher APT repository signing key and sources list.",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in extracted control", want)
		}
	}
}
