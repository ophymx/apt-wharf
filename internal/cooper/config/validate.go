package config

import (
	"fmt"
	"regexp"
	"strings"
)

// validateSidecar enforces the doc 1 schema from cooper-design.md.
// Network-free: regex compilation and structural checks only.
func validateSidecar(s *Sidecar) error {
	if err := validateSource(&s.Source); err != nil {
		return err
	}
	if err := validateVersionSelection(s); err != nil {
		return err
	}
	if s.Epoch < 0 {
		return fmt.Errorf("epoch must be ≥ 0 (got %d)", s.Epoch)
	}
	if err := validateArches(&s.Source, s.Arches); err != nil {
		return err
	}
	return nil
}

func validateSource(src *Source) error {
	count := 0
	if src.GitHub != nil {
		count++
	}
	if src.JSONURL != nil {
		count++
	}
	switch count {
	case 0:
		return fmt.Errorf("source: must set exactly one of source.github, source.json_url")
	case 1:
		// ok
	default:
		return fmt.Errorf("source: must set exactly one of source.github, source.json_url (got %d)", count)
	}

	if src.GitHub != nil {
		gh := src.GitHub
		owner, name, ok := strings.Cut(gh.Repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("source.github.repo %q must be \"owner/name\"", gh.Repo)
		}
		// Default release → "latest".
		if gh.Release == nil {
			gh.Release = &Release{Latest: true}
		}
		if err := validateRelease(gh.Release); err != nil {
			return err
		}
	}

	if src.JSONURL != nil {
		j := src.JSONURL
		if j.URL == "" {
			return fmt.Errorf("source.json_url.url: required")
		}
		if j.VersionPath == "" {
			return fmt.Errorf("source.json_url.version_path: required (gjson path to the version field)")
		}
		if !strings.HasPrefix(j.URL, "http://") && !strings.HasPrefix(j.URL, "https://") {
			return fmt.Errorf("source.json_url.url %q must use http or https scheme", j.URL)
		}
	}
	return nil
}

func validateRelease(r *Release) error {
	count := 0
	if r.Latest {
		count++
	}
	if r.TagPattern != "" {
		count++
	}
	if r.Tag != "" {
		count++
	}
	if count == 0 {
		return fmt.Errorf("source.github.release: must set one of latest, tag_pattern, tag")
	}
	if count > 1 {
		return fmt.Errorf("source.github.release: latest, tag_pattern, tag are mutually exclusive")
	}
	if r.TagPattern != "" {
		re, err := regexp.Compile(r.TagPattern)
		if err != nil {
			return fmt.Errorf("source.github.release.tag_pattern: %w", err)
		}
		r.tagPatternRE = re
	}
	return nil
}

func validateVersionSelection(s *Sidecar) error {
	// For json_url sources the version comes from
	// source.json_url.version_path directly; the tag-related modes
	// don't apply. version_from must be unset.
	if s.Source.JSONURL != nil {
		if s.VersionFrom != "" {
			return fmt.Errorf("version_from: must be unset when source is json_url (version comes from source.json_url.version_path)")
		}
		if s.VersionRegex != "" {
			return fmt.Errorf("version_regex: not valid when source is json_url")
		}
		if s.VersionTemplate != "" {
			return fmt.Errorf("version_template: not valid when source is json_url (post-v0)")
		}
		return nil
	}

	switch s.VersionFrom {
	case "":
		return fmt.Errorf("version_from: required (one of tag, tag_strip_v, asset_filename, fixed)")
	case VersionFromTag, VersionFromTagStripV, VersionFromAssetFilename, VersionFromFixed:
		// ok
	default:
		return fmt.Errorf("version_from: %q is not one of tag, tag_strip_v, asset_filename, fixed", s.VersionFrom)
	}

	if s.VersionFrom == VersionFromAssetFilename {
		if s.VersionRegex == "" {
			return fmt.Errorf("version_regex: required when version_from is asset_filename")
		}
		if _, err := regexp.Compile(s.VersionRegex); err != nil {
			return fmt.Errorf("version_regex: %w", err)
		}
	} else if s.VersionRegex != "" {
		return fmt.Errorf("version_regex: only valid when version_from is asset_filename")
	}

	if s.VersionFrom == VersionFromFixed && s.VersionTemplate == "" {
		return fmt.Errorf("version_template: required when version_from is fixed")
	}
	return nil
}

// assetSubstPattern matches every ${IDENT} reference in an asset selector
// or URL template. validateArches enforces the per-source-kind allowed
// substitutions rule from cooper-design.md §"Doc 1 schema".
var assetSubstPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func validateArches(src *Source, arches map[string]Arch) error {
	if len(arches) == 0 {
		return fmt.Errorf("arches: must define at least one entry")
	}
	for arch, a := range arches {
		if arch == "" {
			return fmt.Errorf("arches: empty arch key")
		}
		switch {
		case src.GitHub != nil:
			if a.Asset == "" {
				return fmt.Errorf("arches.%s.asset: required for source.github", arch)
			}
			if a.AssetURL != "" {
				return fmt.Errorf("arches.%s.asset_url: not valid for source.github (use `asset` instead)", arch)
			}
			for _, m := range assetSubstPattern.FindAllStringSubmatch(a.Asset, -1) {
				if m[1] != "VERSION" {
					return fmt.Errorf("arches.%s.asset: only ${VERSION} substitution is allowed (saw ${%s})", arch, m[1])
				}
			}
		case src.JSONURL != nil:
			if a.AssetURL == "" {
				return fmt.Errorf("arches.%s.asset_url: required for source.json_url", arch)
			}
			if a.Asset != "" {
				return fmt.Errorf("arches.%s.asset: not valid for source.json_url (use `asset_url` instead)", arch)
			}
			// Scheme check is deferred to runtime: a template that
			// starts with a {gjson.path} placeholder (e.g.
			// `{TBA.0.downloads.linux.link}`) resolves to an http(s)
			// URL only after the JSON body is fetched. RenderAssetURL
			// re-checks scheme on the rendered value.
			for _, m := range assetSubstPattern.FindAllStringSubmatch(a.AssetURL, -1) {
				if m[1] != "VERSION" && m[1] != "ARCH" {
					return fmt.Errorf("arches.%s.asset_url: only ${VERSION} and ${ARCH} substitutions are allowed (saw ${%s})", arch, m[1])
				}
			}
		}
	}
	return nil
}
