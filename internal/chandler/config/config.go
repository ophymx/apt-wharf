// Package config loads and validates chandler YAML recipes.
//
// A chandler.yaml describes a vendor's apt repo bootstrap as structured
// data: which signing keys to fetch, which deb822 source stanzas to
// render, and (optionally) a matrix of (distro, codename) targets that
// fans the YAML out into one .deb per target. See chandler-design.md
// for the full schema.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Config is the parsed top-level chandler YAML, post-validation.
type Config struct {
	Path    string // absolute path to the config file
	Dir     string // parent directory of the config file
	Package Package
	Keys    map[string]Key // slug → key definition
	Targets []Target       // nil/empty when YAML omits targets:
	Sources []Source
}

// Package mirrors the package: block in YAML.
type Package struct {
	Name         string
	Version      string
	Description  string
	Maintainer   string
	Homepage     string
	Depends      []string
	Conflicts    []string
	Replaces     []string
	Provides     []string
	RunAptUpdate bool
}

// Key mirrors a single entry in the keys: map. Exactly one of URL or
// URLs is populated; both empty is a validation error.
type Key struct {
	URL  string
	URLs []string
}

// Target is one (distro, codename) tuple from the matrix.
type Target struct {
	Distro   string
	Codename string
}

// Source mirrors one entry in the sources: list.
type Source struct {
	ID            string
	Types         []string
	URIs          []string
	Suites        []string
	Components    []string
	Architectures []string
	Key           string // references a Keys[] slug
}

// raw* types are the on-the-wire YAML shape; the public Config above is
// the normalized form after validation.
type rawConfig struct {
	Package rawPackage        `yaml:"package"`
	Keys    map[string]rawKey `yaml:"keys"`
	Targets []rawTarget       `yaml:"targets"`
	Sources []rawSource       `yaml:"sources"`
}

type rawPackage struct {
	Name         string   `yaml:"name"`
	Version      string   `yaml:"version"`
	Description  string   `yaml:"description"`
	Maintainer   string   `yaml:"maintainer"`
	Homepage     string   `yaml:"homepage"`
	Depends      []string `yaml:"depends"`
	Conflicts    []string `yaml:"conflicts"`
	Replaces     []string `yaml:"replaces"`
	Provides     []string `yaml:"provides"`
	RunAptUpdate bool     `yaml:"run_apt_update"`
}

type rawKey struct {
	URL  string   `yaml:"url"`
	URLs []string `yaml:"urls"`
}

type rawTarget struct {
	Distro   string `yaml:"distro"`
	Codename string `yaml:"codename"`
}

// stringOrList lets schema fields like uris: accept both a scalar and a
// list in YAML. Decoded form is always a []string slice on the Source.
type stringOrList []string

func (s *stringOrList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*s = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, c := range node.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("expected string, got %v at line %d", c.Kind, c.Line)
			}
			out = append(out, c.Value)
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("expected string or list at line %d", node.Line)
	}
}

type rawSource struct {
	ID            string       `yaml:"id"`
	Types         stringOrList `yaml:"types"`
	URIs          stringOrList `yaml:"uris"`
	Suites        stringOrList `yaml:"suites"`
	Components    stringOrList `yaml:"components"`
	Architectures stringOrList `yaml:"architectures"`
	Key           string       `yaml:"key"`
}

// sourceIDRE pins the filename-safe character set for source IDs. Source
// IDs become filenames (sources.list.d/<id>.sources) so they must be
// constrained to safe characters.
var sourceIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// slugRE pins the same character set for key slugs (also a filename:
// /usr/share/keyrings/<slug>.gpg).
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads, decodes (strict mode — unknown keys reject), and validates
// a chandler.yaml. The returned Config is fully normalized; callers can
// trust every field per the design doc's schema rules.
func Load(path string) (*Config, error) {
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
	var doc rawConfig
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("parse %s: chandler.yaml must be a single YAML document", abs)
	}

	c := &Config{
		Path: abs,
		Dir:  filepath.Dir(abs),
	}
	if err := normalize(&doc, c); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return c, nil
}

func normalize(doc *rawConfig, c *Config) error {
	// package block
	if doc.Package.Name == "" {
		return fmt.Errorf("package.name is required")
	}
	if doc.Package.Version == "" {
		return fmt.Errorf("package.version is required")
	}
	if doc.Package.Description == "" {
		return fmt.Errorf("package.description is required")
	}
	if doc.Package.Maintainer == "" {
		return fmt.Errorf("package.maintainer is required")
	}
	c.Package = Package{
		Name:         doc.Package.Name,
		Version:      doc.Package.Version,
		Description:  doc.Package.Description,
		Maintainer:   doc.Package.Maintainer,
		Homepage:     doc.Package.Homepage,
		Depends:      doc.Package.Depends,
		Conflicts:    doc.Package.Conflicts,
		Replaces:     doc.Package.Replaces,
		Provides:     doc.Package.Provides,
		RunAptUpdate: doc.Package.RunAptUpdate,
	}

	// keys block
	if len(doc.Keys) == 0 {
		return fmt.Errorf("keys: must define at least one key")
	}
	c.Keys = make(map[string]Key, len(doc.Keys))
	keySlugs := make([]string, 0, len(doc.Keys))
	for slug, k := range doc.Keys {
		if !slugRE.MatchString(slug) {
			return fmt.Errorf("keys.%s: slug must match %s", slug, slugRE)
		}
		switch {
		case k.URL != "" && len(k.URLs) > 0:
			return fmt.Errorf("keys.%s: set exactly one of url or urls, not both", slug)
		case k.URL == "" && len(k.URLs) == 0:
			return fmt.Errorf("keys.%s: must set url or urls", slug)
		}
		c.Keys[slug] = Key{URL: k.URL, URLs: append([]string(nil), k.URLs...)}
		keySlugs = append(keySlugs, slug)
	}
	sort.Strings(keySlugs)

	// targets block
	if len(doc.Targets) > 0 {
		c.Targets = make([]Target, 0, len(doc.Targets))
		seen := make(map[Target]bool, len(doc.Targets))
		for i, t := range doc.Targets {
			if t.Distro == "" {
				return fmt.Errorf("targets[%d].distro is required", i)
			}
			if t.Codename == "" {
				return fmt.Errorf("targets[%d].codename is required", i)
			}
			tg := Target{Distro: t.Distro, Codename: t.Codename}
			if seen[tg] {
				return fmt.Errorf("targets[%d] duplicates an earlier entry %+v", i, tg)
			}
			seen[tg] = true
			c.Targets = append(c.Targets, tg)
		}
	}

	// sources block
	if len(doc.Sources) == 0 {
		return fmt.Errorf("sources: must list at least one entry")
	}
	c.Sources = make([]Source, 0, len(doc.Sources))
	seenIDs := make(map[string]int, len(doc.Sources))
	for i, s := range doc.Sources {
		if s.ID == "" {
			return fmt.Errorf("sources[%d].id is required", i)
		}
		if !sourceIDRE.MatchString(s.ID) {
			return fmt.Errorf("sources[%d].id %q must match %s", i, s.ID, sourceIDRE)
		}
		if prev, dup := seenIDs[s.ID]; dup {
			return fmt.Errorf("sources[%d].id %q duplicates sources[%d]", i, s.ID, prev)
		}
		seenIDs[s.ID] = i
		if len(s.URIs) == 0 {
			return fmt.Errorf("sources[%d] (%s): uris is required", i, s.ID)
		}
		if len(s.Suites) == 0 {
			return fmt.Errorf("sources[%d] (%s): suites is required", i, s.ID)
		}
		if len(s.Components) == 0 {
			return fmt.Errorf("sources[%d] (%s): components is required", i, s.ID)
		}
		if s.Key == "" {
			return fmt.Errorf("sources[%d] (%s): key is required", i, s.ID)
		}
		if _, ok := c.Keys[s.Key]; !ok {
			return fmt.Errorf("sources[%d] (%s): key %q is not defined in keys:", i, s.ID, s.Key)
		}
		types := []string(s.Types)
		if len(types) == 0 {
			types = []string{"deb"}
		}
		c.Sources = append(c.Sources, Source{
			ID:            s.ID,
			Types:         types,
			URIs:          []string(s.URIs),
			Suites:        []string(s.Suites),
			Components:    []string(s.Components),
			Architectures: []string(s.Architectures),
			Key:           s.Key,
		})
	}

	// referenced-key check (every keys[] entry must be used)
	used := make(map[string]bool, len(c.Keys))
	for _, s := range c.Sources {
		used[s.Key] = true
	}
	for _, slug := range keySlugs {
		if !used[slug] {
			return fmt.Errorf("keys.%s: defined but no sources[] entry references it", slug)
		}
	}

	return nil
}
