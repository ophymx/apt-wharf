package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Top is the parsed cooper.yaml.
type Top struct {
	GitHub   GitHubConfig
	Packages []string // absolute paths to per-package multi-doc files
	Dir      string   // directory of cooper.yaml
}

// GitHubConfig is the github: section of cooper.yaml. token_env and
// token_file are mutually exclusive; if neither is set, cooper runs
// unauthenticated against the GitHub API.
type GitHubConfig struct {
	TokenEnv  string
	TokenFile string
}

type topRaw struct {
	GitHub   *githubRaw `yaml:"github"`
	Packages []string   `yaml:"packages"`
}

type githubRaw struct {
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
}

// LoadTop reads, decodes (strict), and validates a cooper.yaml. Relative
// paths in packages: are resolved against the file's directory.
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
		return nil, fmt.Errorf("parse %s: cooper.yaml must be a single YAML document", abs)
	}

	t := &Top{Dir: filepath.Dir(abs)}
	if doc.GitHub != nil {
		if doc.GitHub.TokenEnv != "" && doc.GitHub.TokenFile != "" {
			return nil, fmt.Errorf("%s: github.token_env and github.token_file are mutually exclusive", abs)
		}
		t.GitHub = GitHubConfig{
			TokenEnv:  doc.GitHub.TokenEnv,
			TokenFile: doc.GitHub.TokenFile,
		}
	}

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
		if info.IsDir() {
			return nil, fmt.Errorf("%s: packages[%d] %s is a directory, expected a file", abs, i, p)
		}
		t.Packages = append(t.Packages, resolved)
	}
	return t, nil
}
