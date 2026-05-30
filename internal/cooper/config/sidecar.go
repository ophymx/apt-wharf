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
	Extract         *ExtractLimits  `yaml:"extract"`
}

// ExtractLimits is the optional per-recipe override for the archive-
// extraction safety caps. See cooper-design.md §"Security & sandboxing"
// — defaults apply when the block is absent or a single knob is unset.
// MaxBytes is humanized at the YAML surface ("8GiB", "4096MB", or a
// bare integer byte count); maxBytesParsed is memoized by validate.
type ExtractLimits struct {
	MaxBytes string `yaml:"max_bytes"`
	MaxFiles int    `yaml:"max_files"`

	maxBytesParsed int64 // populated by validateExtract
}

// ResolvedMaxBytes returns the parsed byte cap. 0 means "use default";
// the build-side Extract function already handles that translation.
func (e *ExtractLimits) ResolvedMaxBytes() int64 {
	if e == nil {
		return 0
	}
	return e.maxBytesParsed
}

// ResolvedMaxFiles returns the file-count cap. 0 means "use default."
func (e *ExtractLimits) ResolvedMaxFiles() int {
	if e == nil {
		return 0
	}
	return e.MaxFiles
}

// Source is a discriminated union over discovery backends. Exactly one
// of GitHub / JSONURL / XMLURL / External must be set; validation
// enforces this. For sources still outside this set — typically when
// the recipe needs dynamic control over nfpm.yaml itself — fall back
// to the external-producer pattern per cooper-design.md §"External
// producers".
type Source struct {
	GitHub   *GitHubSource   `yaml:"github"`
	JSONURL  *JSONURLSource  `yaml:"json_url"`
	XMLURL   *XMLURLSource   `yaml:"xml_url"`
	External *ExternalSource `yaml:"external"`
}

// GitHubSource selects a GitHub release. Release defaults to "latest"
// when omitted.
//
// SourceArchive opts the recipe into also fetching the auto-generated
// source archive (https://github.com/<repo>/archive/refs/tags/<tag>.tar.gz)
// alongside any named binary assets. When true, arches[].asset(s)
// becomes optional — recipes that only want the source tarball
// declare arches: {all: {}} and skip the asset selector. See
// cooper-design.md §"Source kinds → github_release".
type GitHubSource struct {
	Repo              string   `yaml:"repo"`
	Release           *Release `yaml:"release"`
	IncludePrerelease bool     `yaml:"include_prerelease"`
	SourceArchive     bool     `yaml:"source_archive"`
}

// JSONURLSource fetches a vendor JSON metadata endpoint, extracts the
// current version via a gjson path, then renders a per-arch asset URL
// from the recipe's arches[].asset_url template (with ${VERSION} /
// ${ARCH} substitutions and signpost-compatible {token}/{gjson.path}
// placeholders).
//
// Used for vendors that publish download metadata as JSON without a
// GitHub release (golang, node, python, jetbrains-toolbox, ...).
// Mirrors signpost's `json_url` discovery type.
type JSONURLSource struct {
	URL         string `yaml:"url"`
	VersionPath string `yaml:"version_path"`

	// VersionStripPrefix, when non-empty, is removed from the start
	// of the version string after gjson extraction. Useful for
	// vendors whose JSON returns e.g. "go1.26.3" or "v3.13.0" — the
	// raw value isn't a valid Debian upstream version (must start
	// with a digit). Stripping happens before grammar validation.
	VersionStripPrefix string `yaml:"version_strip_prefix"`
}

// XMLURLSource is the XML counterpart to JSONURLSource. The endpoint
// returns XML (JetBrains' updates.xml is the canonical case; RSS / Atom
// feeds and Apache project release feeds fit the same shape); the
// version is plucked via an XPath expression in place of gjson.
//
// arches[].asset_url templates support cooper's standard ${VERSION} /
// ${ARCH} substitutions plus {token} (resolves to the version) and
// {xpath:...} placeholders (run any XPath against the same response
// body, mirroring json_url's {gjson.path} slot). Mirrors signpost's
// `xml_url` discovery type.
type XMLURLSource struct {
	URL          string `yaml:"url"`
	VersionXPath string `yaml:"version_xpath"`

	// VersionStripPrefix has the same semantics as on JSONURLSource:
	// removed from the start of the extracted version. Some vendor
	// feeds prefix their version strings ("v2024.1.4") in a way that's
	// invalid as a Debian upstream version.
	VersionStripPrefix string `yaml:"version_strip_prefix"`
}

// ExternalSource exec's a user-supplied discovery script. The script
// emits {version, assets[{arch, url, sha256}]} on stdout; cooper owns
// build_inputs_hash, source_date_epoch, aux_files, BuildPlan rendering
// downstream. See cooper-design.md §"External source script contract".
type ExternalSource struct {
	Command    []string          `yaml:"command"`
	Env        map[string]string `yaml:"env"`
	EnvForward []string          `yaml:"env_forward"`
	Timeout    string            `yaml:"timeout"`
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

// Arch is one entry of doc 1's arches: map. The recipe picks exactly
// one of singular / plural per source kind; canonicalizeAssets folds
// the singular forms into Assets / AssetURLs at load time so downstream
// code only deals with the plural slices.
//
//   - github_release: Asset / Assets are exact-match release-asset
//     names (`hugo_${VERSION}_linux-amd64.tar.gz`). Plural form covers
//     the cfssl-shape: one release ships N independent binaries with
//     no bundling archive.
//   - json_url:       AssetURL / AssetURLs are fully-qualified download
//     URL templates (`https://example.com/foo-${VERSION}-${ARCH}.tar.gz`,
//     with optional signpost-style {token} / {gjson.path} placeholders
//     against the JSON body).
//   - xml_url:        AssetURL / AssetURLs are URL templates with the
//     same ${VERSION} / ${ARCH} substitutions plus {token} / {xpath:...}
//     placeholders against the XML body.
//
// The singular forms remain syntactic sugar for the single-asset case
// (the overwhelmingly common shape). Validation rejects setting both
// singular and plural in the same arch entry.
type Arch struct {
	Asset     string   `yaml:"asset"`
	AssetURL  string   `yaml:"asset_url"`
	Assets    []string `yaml:"assets"`
	AssetURLs []string `yaml:"asset_urls"`
}
