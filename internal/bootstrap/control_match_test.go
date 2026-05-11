package bootstrap

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"pault.ag/go/debian/deb"
)

// TestBuild_DebControlMatchesPackagesIndex verifies the Version emitted by
// nfpm inside the .deb's control file is bit-identical to the Version we
// pass in. A drift here is what causes apt to forever show the bootstrap
// package as upgradable: dpkg records V_installed from the .deb's control,
// apt checks V_advertised from Packages, and if they differ apt thinks
// there's an upgrade waiting.
func TestBuild_DebControlMatchesPackagesIndex(t *testing.T) {
	in := &Inputs{
		PackageName:   "local-archive-keyring",
		Maintainer:    "Local Ops <ops@localhost>",
		Description:   "Local APT keyring\n",
		BaseURL:       "http://localhost:8080",
		Codename:      "stable",
		Components:    []string{"main"},
		Architectures: []string{"amd64", "arm64"},
		KeyringBytes:  []byte("fake-keyring-bytes"),
	}
	wantVersion := "2026.05.09.1"
	body, _, err := Build(in, wantVersion)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Walk ar archive → control.tar.* → ./control.
	ar, err := deb.LoadAr(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("LoadAr: %v", err)
	}
	var control []byte
	for {
		entry, err := ar.Next()
		if err != nil {
			break
		}
		if !strings.HasPrefix(entry.Name, "control.tar") {
			continue
		}
		tr, closer, err := entry.Tarfile()
		if err != nil {
			t.Fatalf("Tarfile: %v", err)
		}
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			name := strings.TrimPrefix(hdr.Name, "./")
			if name != "control" {
				continue
			}
			buf := make([]byte, hdr.Size)
			if _, err := tr.Read(buf); err != nil && err.Error() != "EOF" {
				t.Fatalf("read control: %v", err)
			}
			control = buf
		}
		closer.Close()
	}
	if control == nil {
		t.Fatal("control file not found in built .deb")
	}

	t.Logf("control file inside .deb:\n%s", control)

	// Pull Version: line.
	gotVersion := ""
	gotPackage := ""
	gotArch := ""
	for line := range strings.SplitSeq(string(control), "\n") {
		switch {
		case strings.HasPrefix(line, "Version: "):
			gotVersion = strings.TrimPrefix(line, "Version: ")
		case strings.HasPrefix(line, "Package: "):
			gotPackage = strings.TrimPrefix(line, "Package: ")
		case strings.HasPrefix(line, "Architecture: "):
			gotArch = strings.TrimPrefix(line, "Architecture: ")
		}
	}

	t.Logf("parsed: Package=%q Version=%q Architecture=%q", gotPackage, gotVersion, gotArch)

	if gotVersion != wantVersion {
		t.Errorf("DEB Version=%q; Packages will say Version=%q — mismatch causes apt to show phantom upgrade",
			gotVersion, wantVersion)
	}
	if gotPackage != in.PackageName {
		t.Errorf("DEB Package=%q want %q", gotPackage, in.PackageName)
	}
	if gotArch != "all" {
		t.Errorf("DEB Architecture=%q want all", gotArch)
	}

	// Print full stanza for human inspection if anything unexpected shows up.
	fmt.Printf("--- control inside .deb ---\n%s---\n", control)
}
