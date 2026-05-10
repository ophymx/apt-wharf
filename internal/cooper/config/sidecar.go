package config

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Version source kinds (cooper-design.md §"Version selection").
const (
	VersionFromTag           = "tag"
	VersionFromTagStripV     = "tag_strip_v"
	VersionFromAssetFilename = "asset_filename"
	VersionFromFixed         = "fixed"
)

// Sidecar is doc 1 of a per-package multi-doc YAML file.
type Sidecar struct {
	Source          Source          `yaml:"source"`
	VersionFrom     string          `yaml:"version_from"`
	VersionTemplate string          `yaml:"version_template"`
	VersionRegex    string          `yaml:"version_regex"`
	Epoch           int             `yaml:"epoch"`
	Arches          map[string]Arch `yaml:"arches"`
}

// Source is a discriminated union over discovery backends. Cooper ships
// only the GitHub backend; non-GitHub sources go through external
// producers per cooper-design.md §"External producers".
type Source struct {
	GitHub *GitHubSource `yaml:"github"`
}

// GitHubSource selects a GitHub release. Release defaults to "latest"
// when omitted.
type GitHubSource struct {
	Repo              string   `yaml:"repo"`
	Release           *Release `yaml:"release"`
	IncludePrerelease bool     `yaml:"include_prerelease"`
}

// Release is a flat-union: exactly one of Latest, TagPattern, Tag is
// populated after parsing+defaulting.
type Release struct {
	Latest     bool
	TagPattern string
	Tag        string

	tagPatternRE *regexp.Regexp // memoized at validation time
}

// CompiledTagPattern returns the regex compiled at validate time. nil
// when Release is not in tag_pattern form.
func (r *Release) CompiledTagPattern() *regexp.Regexp { return r.tagPatternRE }

// UnmarshalYAML accepts either a scalar "latest" or a mapping containing
// exactly one of {tag_pattern, tag}.
func (r *Release) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return fmt.Errorf("release: %w", err)
		}
		if s != "latest" {
			return fmt.Errorf("release: scalar must be \"latest\" (got %q)", s)
		}
		r.Latest = true
		return nil
	case yaml.MappingNode:
		// Walk the mapping manually so we can reject unknown keys at this
		// level (yaml.v3's KnownFields only applies through a Decoder,
		// not through Node.Decode).
		var pattern, tag string
		seen := map[string]int{}
		for i := 0; i < len(value.Content); i += 2 {
			k := value.Content[i]
			v := value.Content[i+1]
			switch k.Value {
			case "tag_pattern":
				if err := v.Decode(&pattern); err != nil {
					return fmt.Errorf("release.tag_pattern: %w", err)
				}
			case "tag":
				if err := v.Decode(&tag); err != nil {
					return fmt.Errorf("release.tag: %w", err)
				}
			default:
				return fmt.Errorf("release: unknown key %q (allowed: tag_pattern, tag)", k.Value)
			}
			seen[k.Value]++
		}
		if seen["tag_pattern"] > 0 && seen["tag"] > 0 {
			return fmt.Errorf("release: tag_pattern and tag are mutually exclusive")
		}
		switch {
		case pattern != "":
			r.TagPattern = pattern
		case tag != "":
			r.Tag = tag
		default:
			return fmt.Errorf("release: mapping must set tag_pattern or tag")
		}
		return nil
	default:
		return fmt.Errorf("release: must be scalar \"latest\" or a mapping (got node kind %d)", value.Kind)
	}
}

// Arch is one entry of doc 1's arches: map.
type Arch struct {
	Asset string `yaml:"asset"`
}
