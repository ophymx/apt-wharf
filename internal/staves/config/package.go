package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// PackageFile is a parsed per-package staves recipe.
//
// Staves recipes are pure nfpm.yaml (no cooper-style sidecar) since
// there's no upstream resolution to do. NfpmName and Version are
// extracted at load time because staves uses them to name .deb files
// and to populate plan.Artifact fields.
type PackageFile struct {
	Path     string    // absolute path to the nfpm.yaml
	Dir      string    // package directory (parent of nfpm.yaml)
	Nfpm     yaml.Node // doc held verbatim for downstream re-emission
	NfpmName string
	Version  string
	Arch     string // optional in nfpm; defaults to "all" if absent
}

// debianVersionRE pins the same upstream-version grammar cooper's
// version package uses (Debian Policy §5.6.12 minus debian-revision).
// Duplicated here so staves stays independent of cooper internals.
var debianVersionRE = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)

// LoadPackage reads and validates one staves package recipe. pkgDir
// is the directory containing nfpm.yaml.
func LoadPackage(pkgDir string) (*PackageFile, error) {
	abs, err := filepath.Abs(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", pkgDir, err)
	}
	path := filepath.Join(abs, "nfpm.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var n yaml.Node
	if err := yaml.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// A second doc is illegal — staves nfpm.yaml is single-doc only.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for i := 0; ; i++ {
		var skip yaml.Node
		if err := dec.Decode(&skip); err != nil {
			break
		}
		if i >= 1 {
			return nil, fmt.Errorf("%s: nfpm.yaml must be a single YAML document", path)
		}
	}

	name, err := mappingString(&n, "name")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if name == "" {
		return nil, fmt.Errorf("%s: name: required", path)
	}
	version, err := mappingString(&n, "version")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if version == "" {
		return nil, fmt.Errorf("%s: version: required (staves does not derive versions; pin in nfpm.yaml)", path)
	}
	if !debianVersionRE.MatchString(version) {
		return nil, fmt.Errorf("%s: version %q does not match Debian's upstream-version grammar `^[0-9][A-Za-z0-9.+~-]*$`",
			path, version)
	}
	arch, err := mappingString(&n, "arch")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if arch == "" {
		arch = "all"
	}

	return &PackageFile{
		Path:     path,
		Dir:      abs,
		Nfpm:     n,
		NfpmName: name,
		Version:  version,
		Arch:     arch,
	}, nil
}

// mappingString returns the scalar string at key in the top-level
// mapping of doc. Returns "" if the key is absent. Errors only on a
// non-string value at that key.
func mappingString(doc *yaml.Node, key string) (string, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return "", fmt.Errorf("expected exactly one root node, got %d", len(root.Content))
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return "", fmt.Errorf("nfpm.yaml root must be a mapping")
	}
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value == key {
			if v.Kind != yaml.ScalarNode {
				return "", fmt.Errorf("%s: must be a scalar string", key)
			}
			return v.Value, nil
		}
	}
	return "", nil
}
