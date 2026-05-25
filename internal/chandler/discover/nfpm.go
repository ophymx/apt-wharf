package discover

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ophymx/apt-signpost/internal/chandler/config"
)

// nfpmSubtree is the resolved nfpm.yaml shape chandler emits per
// matrix target. Field order matches the on-disk YAML reading order
// nfpm users expect, but JCS canonicalization in build_inputs_hash
// sorts keys alphabetically — so order here doesn't affect the hash.
type nfpmSubtree struct {
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Arch        string        `json:"arch"`
	Platform    string        `json:"platform"`
	Section     string        `json:"section"`
	Priority    string        `json:"priority"`
	Maintainer  string        `json:"maintainer"`
	Description string        `json:"description"`
	Homepage    string        `json:"homepage,omitempty"`
	Depends     []string      `json:"depends,omitempty"`
	Conflicts   []string      `json:"conflicts,omitempty"`
	Replaces    []string      `json:"replaces,omitempty"`
	Provides    []string      `json:"provides,omitempty"`
	Contents    []nfpmContent `json:"contents"`
	Scripts     *nfpmScripts  `json:"scripts,omitempty"`
}

type nfpmContent struct {
	Src      string        `json:"src"`
	Dst      string        `json:"dst"`
	Type     string        `json:"type,omitempty"` // "config" for .sources files
	FileInfo *nfpmFileInfo `json:"file_info,omitempty"`
}

type nfpmFileInfo struct {
	Mode int `json:"mode"`
}

type nfpmScripts struct {
	Postinstall string `json:"postinstall,omitempty"`
}

// aptDependsLowerBound is auto-injected as the apt version where deb822
// .sources files and Signed-By: support stabilized.
const aptDependsLowerBound = "apt (>= 1.5)"

// buildNfpm assembles the nfpm subtree for one matrix target. The
// contents list is sorted alphabetically by destination path for
// deterministic output: /etc/... entries sort before /usr/... entries,
// and within each prefix the slug/source-id order resolves
// lexicographically.
func buildNfpm(cfg *config.Config, resolvedName string, keySlugs, sourceIDs []string) (json.RawMessage, error) {
	contents := make([]nfpmContent, 0, len(keySlugs)+len(sourceIDs))
	// Source files (.sources) → /etc/apt/sources.list.d/, conffile.
	for _, id := range sourceIDs {
		contents = append(contents, nfpmContent{
			Src:      "./" + id + ".sources",
			Dst:      "/etc/apt/sources.list.d/" + id + ".sources",
			Type:     "config",
			FileInfo: &nfpmFileInfo{Mode: 0o644},
		})
	}
	// Keyring files (.gpg) → /usr/share/keyrings/, not conffile.
	for _, slug := range keySlugs {
		contents = append(contents, nfpmContent{
			Src:      "./" + slug + ".gpg",
			Dst:      "/usr/share/keyrings/" + slug + ".gpg",
			FileInfo: &nfpmFileInfo{Mode: 0o644},
		})
	}
	sort.SliceStable(contents, func(i, j int) bool { return contents[i].Dst < contents[j].Dst })

	depends := mergeDepends(cfg.Package.Depends)

	out := nfpmSubtree{
		Name:        resolvedName,
		Version:     cfg.Package.Version,
		Arch:        "all",
		Platform:    "linux",
		Section:     "misc",
		Priority:    "optional",
		Maintainer:  cfg.Package.Maintainer,
		Description: cfg.Package.Description,
		Homepage:    cfg.Package.Homepage,
		Depends:     depends,
		Conflicts:   cfg.Package.Conflicts,
		Replaces:    cfg.Package.Replaces,
		Provides:    cfg.Package.Provides,
		Contents:    contents,
	}
	if cfg.Package.RunAptUpdate {
		out.Scripts = &nfpmScripts{Postinstall: "./postinst.sh"}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal nfpm subtree: %w", err)
	}
	return raw, nil
}

// mergeDepends prepends the auto-injected apt floor to the user's
// depends list, deduplicating if the user already named it.
func mergeDepends(userDepends []string) []string {
	out := []string{aptDependsLowerBound}
	for _, d := range userDepends {
		if d == aptDependsLowerBound {
			continue
		}
		out = append(out, d)
	}
	return out
}
