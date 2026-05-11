package config

import (
	"fmt"
	"regexp"
	"strings"
)

// validateSidecar enforces the doc 1 schema from cooper-design.md.
// Network-free: regex compilation and structural checks only.
//
// validateArches also normalizes Arch entries in place: singular asset /
// asset_url forms are folded into the corresponding plural slot so
// downstream code only deals with Assets / AssetURLs. The map is
// mutated through reassignment so callers see the canonical shape.
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
	if src.XMLURL != nil {
		count++
	}
	switch count {
	case 0:
		return fmt.Errorf("source: must set exactly one of source.github, source.json_url, source.xml_url")
	case 1:
		// ok
	default:
		return fmt.Errorf("source: must set exactly one of source.github, source.json_url, source.xml_url (got %d)", count)
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

	if src.XMLURL != nil {
		x := src.XMLURL
		if x.URL == "" {
			return fmt.Errorf("source.xml_url.url: required")
		}
		if x.VersionXPath == "" {
			return fmt.Errorf("source.xml_url.version_xpath: required (XPath expression resolving to the version field)")
		}
		if !strings.HasPrefix(x.URL, "http://") && !strings.HasPrefix(x.URL, "https://") {
			return fmt.Errorf("source.xml_url.url %q must use http or https scheme", x.URL)
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

	// xml_url uses the same model as json_url: version comes from
	// source.xml_url.version_xpath; the tag-related modes don't apply.
	if s.Source.XMLURL != nil {
		if s.VersionFrom != "" {
			return fmt.Errorf("version_from: must be unset when source is xml_url (version comes from source.xml_url.version_xpath)")
		}
		if s.VersionRegex != "" {
			return fmt.Errorf("version_regex: not valid when source is xml_url")
		}
		if s.VersionTemplate != "" {
			return fmt.Errorf("version_template: not valid when source is xml_url (post-v0)")
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
		// Singular/plural mutex per kind. Singular forms are sugar for
		// a single-entry plural slice; setting both at once is a recipe
		// authoring error.
		if a.Asset != "" && len(a.Assets) > 0 {
			return fmt.Errorf("arches.%s: cannot set both asset and assets (singular is sugar for a single-entry plural slice)", arch)
		}
		if a.AssetURL != "" && len(a.AssetURLs) > 0 {
			return fmt.Errorf("arches.%s: cannot set both asset_url and asset_urls", arch)
		}

		switch {
		case src.GitHub != nil:
			if a.AssetURL != "" || len(a.AssetURLs) > 0 {
				return fmt.Errorf("arches.%s: asset_url/asset_urls not valid for source.github (use asset/assets instead)", arch)
			}
			// Canonicalize singular → plural.
			if a.Asset != "" {
				a.Assets = []string{a.Asset}
				a.Asset = ""
			}
			if len(a.Assets) == 0 {
				return fmt.Errorf("arches.%s.asset(s): required for source.github", arch)
			}
			for i, asset := range a.Assets {
				if asset == "" {
					return fmt.Errorf("arches.%s.assets[%d]: empty entry", arch, i)
				}
				for _, m := range assetSubstPattern.FindAllStringSubmatch(asset, -1) {
					if m[1] != "VERSION" {
						return fmt.Errorf("arches.%s.assets[%d]: only ${VERSION} substitution is allowed (saw ${%s})", arch, i, m[1])
					}
				}
			}
			if err := checkDuplicateTemplates(arch, "assets", a.Assets); err != nil {
				return err
			}
		case src.JSONURL != nil:
			if a.Asset != "" || len(a.Assets) > 0 {
				return fmt.Errorf("arches.%s: asset/assets not valid for source.json_url (use asset_url/asset_urls instead)", arch)
			}
			if a.AssetURL != "" {
				a.AssetURLs = []string{a.AssetURL}
				a.AssetURL = ""
			}
			if len(a.AssetURLs) == 0 {
				return fmt.Errorf("arches.%s.asset_url(s): required for source.json_url", arch)
			}
			// Scheme check is deferred to runtime: a template that
			// starts with a {gjson.path} placeholder (e.g.
			// `{TBA.0.downloads.linux.link}`) resolves to an http(s)
			// URL only after the JSON body is fetched. RenderAssetURL
			// re-checks scheme on the rendered value.
			for i, u := range a.AssetURLs {
				if u == "" {
					return fmt.Errorf("arches.%s.asset_urls[%d]: empty entry", arch, i)
				}
				for _, m := range assetSubstPattern.FindAllStringSubmatch(u, -1) {
					if m[1] != "VERSION" && m[1] != "ARCH" {
						return fmt.Errorf("arches.%s.asset_urls[%d]: only ${VERSION} and ${ARCH} substitutions are allowed (saw ${%s})", arch, i, m[1])
					}
				}
			}
			if err := checkDuplicateTemplates(arch, "asset_urls", a.AssetURLs); err != nil {
				return err
			}
		case src.XMLURL != nil:
			if a.Asset != "" || len(a.Assets) > 0 {
				return fmt.Errorf("arches.%s: asset/assets not valid for source.xml_url (use asset_url/asset_urls instead)", arch)
			}
			if a.AssetURL != "" {
				a.AssetURLs = []string{a.AssetURL}
				a.AssetURL = ""
			}
			if len(a.AssetURLs) == 0 {
				return fmt.Errorf("arches.%s.asset_url(s): required for source.xml_url", arch)
			}
			// Same deferred-scheme-check rationale as json_url: a
			// template that opens with `{xpath:...}` only resolves to
			// an http(s) URL once the XML body has been fetched. The
			// runtime renderer re-validates scheme on the result.
			for i, u := range a.AssetURLs {
				if u == "" {
					return fmt.Errorf("arches.%s.asset_urls[%d]: empty entry", arch, i)
				}
				for _, m := range assetSubstPattern.FindAllStringSubmatch(u, -1) {
					if m[1] != "VERSION" && m[1] != "ARCH" {
						return fmt.Errorf("arches.%s.asset_urls[%d]: only ${VERSION} and ${ARCH} substitutions are allowed (saw ${%s})", arch, i, m[1])
					}
				}
			}
			if err := checkDuplicateTemplates(arch, "asset_urls", a.AssetURLs); err != nil {
				return err
			}
		}
		arches[arch] = a
	}
	return nil
}

// checkDuplicateTemplates rejects two entries in the same plural slice
// whose verbatim templates match. We can't catch every name-collision
// case at validate time — placeholders (${VERSION}, {gjson.path},
// {xpath:...}) only resolve at discover time, and the rendered basenames
// are what land under ${ASSETS}/. But identical templates are a
// guaranteed collision; rejecting them up-front is cheap and prevents
// recipes that obviously can't work.
func checkDuplicateTemplates(arch, field string, entries []string) error {
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		if prev, ok := seen[e]; ok {
			return fmt.Errorf("arches.%s.%s: duplicate entry %q (indices %d and %d)", arch, field, e, prev, i)
		}
		seen[e] = i
	}
	return nil
}
