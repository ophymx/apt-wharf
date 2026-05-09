// Package bootstrap renders the self-published keyring .deb that clients
// `dpkg -i` to trust the apt repository. The .deb is rebuilt only when the
// input hash changes — keeping the version stable across no-op refresh ticks.
package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/files"

	// Register the deb packager.
	_ "github.com/goreleaser/nfpm/v2/deb"
)

// Inputs captures every piece of state that should force a bootstrap rebuild.
type Inputs struct {
	PackageName   string
	Maintainer    string
	Description   string
	BaseURL       string // repository.base_url, no trailing slash
	Codename      string
	Components    []string // ["main"] in MVP
	Architectures []string
	KeyringBytes  []byte // canonical-order public keys (active first)
}

// InputHash returns sha256:HEX over the canonicalized inputs. Any change in
// any field flips the hash; equal hash → reuse cached .deb.
func InputHash(in *Inputs) string {
	h := sha256.New()
	writeField := func(key, val string) {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", key, len(val), val)
	}
	writeField("package_name", in.PackageName)
	writeField("maintainer", in.Maintainer)
	writeField("description", in.Description)
	writeField("base_url", in.BaseURL)
	writeField("codename", in.Codename)

	comps := append([]string{}, in.Components...)
	sort.Strings(comps)
	writeField("components", strings.Join(comps, ","))

	archs := append([]string{}, in.Architectures...)
	sort.Strings(archs)
	writeField("architectures", strings.Join(archs, ","))

	// Keyring bytes are already in canonical order from the signer.
	keyHash := sha256.Sum256(in.KeyringBytes)
	writeField("keyring_sha256", hex.EncodeToString(keyHash[:]))

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// NextVersion produces a YYYY.MM.DD.N version string. If prev shares today's
// date prefix, N increments; otherwise N resets to 1.
func NextVersion(prev string, now time.Time) string {
	today := now.UTC().Format("2006.01.02")
	if prev == "" {
		return today + ".1"
	}
	prefix := today + "."
	if strings.HasPrefix(prev, prefix) {
		nStr := strings.TrimPrefix(prev, prefix)
		if n, err := strconv.Atoi(nStr); err == nil && n > 0 {
			return prefix + strconv.Itoa(n+1)
		}
	}
	return today + ".1"
}

// Build writes a .deb to memory containing the keyring file and the deb822
// .sources stub. Architecture: all. Returns raw .deb bytes plus a SHA256
// over them, both used to populate the snapshot.
func Build(in *Inputs, version string) ([]byte, string, error) {
	if in.PackageName == "" || in.BaseURL == "" || in.Codename == "" {
		return nil, "", fmt.Errorf("bootstrap.Build: missing required inputs")
	}

	tmpDir, err := os.MkdirTemp("", "signpost-bootstrap-*")
	if err != nil {
		return nil, "", fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	keyringPath := filepath.Join(tmpDir, in.PackageName+".gpg")
	if err := os.WriteFile(keyringPath, in.KeyringBytes, 0o644); err != nil {
		return nil, "", fmt.Errorf("write keyring: %w", err)
	}

	sourcesPath := filepath.Join(tmpDir, in.PackageName+".sources")
	sourcesBody := renderDeb822(in)
	if err := os.WriteFile(sourcesPath, sourcesBody, 0o644); err != nil {
		return nil, "", fmt.Errorf("write sources: %w", err)
	}

	info := nfpm.WithDefaults(&nfpm.Info{
		Name:        in.PackageName,
		Arch:        "all",
		Platform:    "linux",
		Version:     version,
		Maintainer:  in.Maintainer,
		Description: in.Description,
		Section:     "misc",
		Priority:    "optional",
		Overridables: nfpm.Overridables{
			Contents: files.Contents{
				{
					Source:      keyringPath,
					Destination: "/usr/share/keyrings/" + in.PackageName + ".gpg",
				},
				{
					Source:      sourcesPath,
					Destination: "/etc/apt/sources.list.d/" + in.PackageName + ".sources",
					Type:        "config|noreplace",
				},
			},
		},
	})
	if err := nfpm.Validate(info); err != nil {
		return nil, "", fmt.Errorf("nfpm validate: %w", err)
	}

	packager, err := nfpm.Get("deb")
	if err != nil {
		return nil, "", fmt.Errorf("nfpm get deb: %w", err)
	}

	var buf bytes.Buffer
	if err := packager.Package(info, &buf); err != nil {
		return nil, "", fmt.Errorf("nfpm package: %w", err)
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// renderDeb822 returns the contents of /etc/apt/sources.list.d/<pkg>.sources.
//
// Format reference: https://manpages.debian.org/bookworm/apt/sources.list.5.en.html
func renderDeb822(in *Inputs) []byte {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "Types: deb")
	fmt.Fprintf(&buf, "URIs: %s\n", in.BaseURL)
	fmt.Fprintf(&buf, "Suites: %s\n", in.Codename)
	fmt.Fprintf(&buf, "Components: %s\n", strings.Join(in.Components, " "))
	fmt.Fprintf(&buf, "Architectures: %s\n", strings.Join(in.Architectures, " "))
	fmt.Fprintf(&buf, "Signed-By: /usr/share/keyrings/%s.gpg\n", in.PackageName)
	return buf.Bytes()
}

// PoolPath is the canonical location of the bootstrap .deb in /pool.
func PoolPath(packageName, version string) string {
	prefix := string(packageName[0])
	return fmt.Sprintf("/pool/main/%s/%s/%s_%s_all.deb", prefix, packageName, packageName, version)
}

// Filename returns just the .deb filename (no leading path).
func Filename(packageName, version string) string {
	return fmt.Sprintf("%s_%s_all.deb", packageName, version)
}
