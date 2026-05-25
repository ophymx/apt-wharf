// Package render produces the on-disk file contents chandler ships in
// a .deb: one deb822 .sources file per source[] entry, plus an
// optional postinst shell script.
//
// All rendering is pure: same inputs → same bytes. Field order is
// fixed so the output is byte-deterministic.
package render

import (
	"fmt"
	"strings"
)

// SourceStanza is the inputs for one rendered .sources file. Every
// list field is rendered space-separated per deb822 convention.
//
// Empty Architectures means "do not render the Architectures: line"
// (apt falls back to dpkg's default arch).
type SourceStanza struct {
	Types         []string
	URIs          []string
	Suites        []string
	Components    []string
	Architectures []string
	SignedBy      string // absolute path to the keyring file
}

// Stanza renders one .sources file body. The result is a complete
// file (no trailing blank), ending with a single newline.
//
// Enabled: yes is always emitted first so operators see the toggle
// at the top of the file; the bare deb822 format has no comment
// header (see chandler-design.md "deb822 stanza rendering").
func Stanza(s SourceStanza) (string, error) {
	if len(s.Types) == 0 {
		return "", fmt.Errorf("render: Types is required")
	}
	if len(s.URIs) == 0 {
		return "", fmt.Errorf("render: URIs is required")
	}
	if len(s.Suites) == 0 {
		return "", fmt.Errorf("render: Suites is required")
	}
	if len(s.Components) == 0 {
		return "", fmt.Errorf("render: Components is required")
	}
	if s.SignedBy == "" {
		return "", fmt.Errorf("render: SignedBy is required")
	}
	var b strings.Builder
	fmt.Fprintln(&b, "Enabled: yes")
	fmt.Fprintf(&b, "Types: %s\n", strings.Join(s.Types, " "))
	fmt.Fprintf(&b, "URIs: %s\n", strings.Join(s.URIs, " "))
	fmt.Fprintf(&b, "Suites: %s\n", strings.Join(s.Suites, " "))
	fmt.Fprintf(&b, "Components: %s\n", strings.Join(s.Components, " "))
	if len(s.Architectures) > 0 {
		fmt.Fprintf(&b, "Architectures: %s\n", strings.Join(s.Architectures, " "))
	}
	fmt.Fprintf(&b, "Signed-By: %s\n", s.SignedBy)
	return b.String(), nil
}

// Postinst returns the shell script chandler ships when
// package.run_apt_update is true. Hardcoded text — no user-controlled
// strings flow into the body.
func Postinst() string {
	return `#!/bin/sh
set -e
if [ "$1" = "configure" ]; then
    if ! apt-get update; then
        echo "warning: apt-get update failed; run 'apt-get update' manually before installing packages from these sources" >&2
    fi
fi
`
}
