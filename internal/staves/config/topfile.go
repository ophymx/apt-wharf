// Package config loads and validates staves recipe files.
//
// Top-level staves.yaml lists package directories. Each package
// directory contains a single nfpm.yaml (vanilla nfpm, no cooper
// sidecar) plus any files referenced from contents[].src and
// scripts.*. There's no upstream resolution to do — every byte
// destined for the .deb is already on disk.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Top is the parsed staves.yaml.
type Top struct {
	Packages []string // absolute paths to per-package directories
	Dir      string   // directory of staves.yaml
}

type topRaw struct {
	Packages []string `yaml:"packages"`
}

// LoadTop reads, decodes (strict), and validates a staves.yaml.
// Each entry in packages: must resolve to an existing directory.
func LoadTop(path string) (*Top, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", abs, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc topRaw
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("parse %s: staves.yaml must be a single YAML document", abs)
	}

	t := &Top{Dir: filepath.Dir(abs)}
	if len(doc.Packages) == 0 {
		return nil, fmt.Errorf("%s: packages: must list at least one path", abs)
	}
	t.Packages = make([]string, 0, len(doc.Packages))
	seen := make(map[string]string, len(doc.Packages))
	for i, p := range doc.Packages {
		if p == "" {
			return nil, fmt.Errorf("%s: packages[%d] is empty", abs, i)
		}
		resolved := p
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(t.Dir, resolved)
		}
		resolved = filepath.Clean(resolved)
		if prev, dup := seen[resolved]; dup {
			return nil, fmt.Errorf("%s: packages[%d] duplicates an earlier entry %s", abs, i, prev)
		}
		seen[resolved] = p
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("%s: packages[%d] %s: %w", abs, i, p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: packages[%d] %s must be a directory containing nfpm.yaml", abs, i, p)
		}
		if _, err := os.Stat(filepath.Join(resolved, "nfpm.yaml")); err != nil {
			return nil, fmt.Errorf("%s: packages[%d] %s: missing nfpm.yaml: %w", abs, i, p, err)
		}
		t.Packages = append(t.Packages, resolved)
	}
	return t, nil
}
