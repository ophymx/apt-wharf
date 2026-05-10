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
	if err := validateArches(s.Arches); err != nil {
		return err
	}
	return nil
}

func validateSource(src *Source) error {
	if src.GitHub == nil {
		return fmt.Errorf("source: missing source.github (the only built-in backend)")
	}
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

// assetSubstPattern matches every ${IDENT} reference in an asset selector.
// validateArches uses it to enforce the "${VERSION} is the only allowed
// substitution" rule from cooper-design.md §"Doc 1 schema".
var assetSubstPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func validateArches(arches map[string]Arch) error {
	if len(arches) == 0 {
		return fmt.Errorf("arches: must define at least one entry")
	}
	for arch, a := range arches {
		if arch == "" {
			return fmt.Errorf("arches: empty arch key")
		}
		if a.Asset == "" {
			return fmt.Errorf("arches.%s.asset: required", arch)
		}
		for _, m := range assetSubstPattern.FindAllStringSubmatch(a.Asset, -1) {
			if m[1] != "VERSION" {
				return fmt.Errorf("arches.%s.asset: only ${VERSION} substitution is allowed (saw ${%s})", arch, m[1])
			}
		}
	}
	return nil
}
