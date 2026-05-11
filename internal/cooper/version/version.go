package version

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
)

// debianVersionRE is the upstream-version grammar from Debian Policy:
// must start with a digit, then any of [A-Za-z0-9.+~-]. Cooper's
// resolved versions are always upstream-only (no Debian revision); the
// epoch is glued in elsewhere.
var debianVersionRE = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)

// curlyPattern matches {name} substitutions inside version_template.
// Note: deliberately distinct from doc 2's ${VAR} syntax so the two
// surfaces don't collide.
var curlyPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Assemble computes the resolved ${VERSION} string for one package and
// the GitHub release it points at.
//
// For asset_filename mode, the first asset whose name matches
// sidecar.VersionRegex provides the captures used as substitutions
// (named groups) and as the fallback base value (first numbered group).
// All arches in a typical package land at the same captured value, so
// "first match wins" is deterministic per release.
func Assemble(sidecar *config.Sidecar, release *github.RepositoryRelease) (string, error) {
	tag := release.GetTagName()

	subs := map[string]string{
		"tag":         tag,
		"tag_strip_v": strings.TrimPrefix(tag, "v"),
		"date":        release.GetPublishedAt().UTC().Format("20060102"),
	}

	assetFirstCapture := ""
	if sidecar.VersionFrom == config.VersionFromAssetFilename {
		re, err := regexp.Compile(sidecar.VersionRegex)
		if err != nil {
			return "", fmt.Errorf("version_regex: %w", err)
		}
		var match []string
		for _, a := range release.Assets {
			if m := re.FindStringSubmatch(a.GetName()); m != nil {
				match = m
				break
			}
		}
		if match == nil {
			return "", fmt.Errorf("version_regex %s matched no asset names in release %s",
				sidecar.VersionRegex, tag)
		}
		if len(match) >= 2 {
			assetFirstCapture = match[1]
		}
		for i, n := range re.SubexpNames() {
			if n == "" || i >= len(match) {
				continue
			}
			subs[n] = match[i]
		}
	}

	var resolved string
	switch {
	case sidecar.VersionTemplate != "":
		out, err := applyCurly(sidecar.VersionTemplate, subs)
		if err != nil {
			return "", err
		}
		resolved = out
	case sidecar.VersionFrom == config.VersionFromTag:
		resolved = tag
	case sidecar.VersionFrom == config.VersionFromTagStripV:
		resolved = subs["tag_strip_v"]
	case sidecar.VersionFrom == config.VersionFromAssetFilename:
		if assetFirstCapture == "" {
			return "", fmt.Errorf("version_regex %s needs at least one capture group when version_template is unset",
				sidecar.VersionRegex)
		}
		resolved = assetFirstCapture
	case sidecar.VersionFrom == config.VersionFromFixed:
		return "", fmt.Errorf("version_from: fixed requires version_template")
	default:
		return "", fmt.Errorf("unrecognized version_from %q", sidecar.VersionFrom)
	}

	if !MatchesDebianGrammar(resolved) {
		return "", fmt.Errorf("resolved version %q does not match Debian's ^[0-9][A-Za-z0-9.+~-]*$",
			resolved)
	}
	return resolved, nil
}

// MatchesDebianGrammar reports whether v conforms to cooper's
// upstream-version grammar (Debian Policy §5.6.12, minus debian-revision):
// starts with a digit, then any of `[A-Za-z0-9.+~-]`. Exported so
// non-github source backends (json_url, future kinds) can validate
// their extracted versions against the same rule.
func MatchesDebianGrammar(v string) bool {
	return debianVersionRE.MatchString(v)
}

// applyCurly substitutes {name} placeholders in s using subs. Unknown
// names accumulate into a single error so users get the full list at
// once rather than one-typo-per-run.
func applyCurly(s string, subs map[string]string) (string, error) {
	var missing []string
	out := curlyPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := subs[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown substitution(s) in version_template: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
