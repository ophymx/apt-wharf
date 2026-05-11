package source

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
)

// ResolveRelease fetches the GitHub release identified by gh. The three
// release modes from cooper-design.md §"Doc 1 schema" map to:
//
//   - Latest=true, IncludePrerelease=false → GET /repos/.../releases/latest
//   - Latest=true, IncludePrerelease=true  → list, take the first non-draft
//   - TagPattern set                       → list, take the first non-draft
//     whose tag matches the pattern
//     (and skip prereleases unless
//     IncludePrerelease is true)
//   - Tag set                              → GET /repos/.../releases/tags/<tag>
//
// One API call per package is the goal; the caller passes the same
// release into MatchAsset for every arch.
func ResolveRelease(ctx context.Context, client *github.Client, gh *config.GitHubSource) (*github.RepositoryRelease, error) {
	owner, name, ok := strings.Cut(gh.Repo, "/")
	if !ok {
		return nil, fmt.Errorf("invalid repo %q", gh.Repo)
	}

	switch {
	case gh.Release.Tag != "":
		rel, resp, err := client.Repositories.GetReleaseByTag(ctx, owner, name, gh.Release.Tag)
		if err != nil {
			return nil, fmt.Errorf("GetReleaseByTag %s/%s %s: %w", owner, name, gh.Release.Tag, err)
		}
		if resp != nil && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GetReleaseByTag %s/%s %s: status %s", owner, name, gh.Release.Tag, resp.Status)
		}
		return rel, nil

	case gh.Release.Latest && !gh.IncludePrerelease:
		rel, resp, err := client.Repositories.GetLatestRelease(ctx, owner, name)
		if err != nil {
			return nil, fmt.Errorf("GetLatestRelease %s/%s: %w", owner, name, err)
		}
		if resp != nil && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GetLatestRelease %s/%s: status %s", owner, name, resp.Status)
		}
		return rel, nil

	case gh.Release.Latest && gh.IncludePrerelease:
		return firstMatching(ctx, client, owner, name, true, nil)

	case gh.Release.TagPattern != "":
		re := gh.Release.CompiledTagPattern()
		if re == nil {
			return nil, fmt.Errorf("tag_pattern not compiled (config bug?)")
		}
		return firstMatching(ctx, client, owner, name, gh.IncludePrerelease, func(tag string) bool {
			return re.MatchString(tag)
		})

	default:
		return nil, fmt.Errorf("invalid release union (validate should have caught this)")
	}
}

// firstMatching lists releases (newest first per GitHub API ordering),
// skips drafts, optionally skips prereleases, and applies an optional
// tag filter. Returns the first non-draft release that passes both
// filters.
//
// Page size 30 covers the common case of "latest matching release within
// the last few cuts"; we don't paginate beyond that — packages stuck on
// stale tags should pin via tag: rather than tag_pattern.
func firstMatching(
	ctx context.Context,
	client *github.Client,
	owner, name string,
	includePrerelease bool,
	tagFilter func(string) bool,
) (*github.RepositoryRelease, error) {
	releases, resp, err := client.Repositories.ListReleases(ctx, owner, name, &github.ListOptions{PerPage: 30})
	if err != nil {
		return nil, fmt.Errorf("ListReleases %s/%s: %w", owner, name, err)
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListReleases %s/%s: status %s", owner, name, resp.Status)
	}
	for _, r := range releases {
		if r.GetDraft() {
			continue
		}
		if !includePrerelease && r.GetPrerelease() {
			continue
		}
		if tagFilter != nil && !tagFilter(r.GetTagName()) {
			continue
		}
		return r, nil
	}
	return nil, fmt.Errorf("no release in %s/%s matched the configured filters", owner, name)
}

// MatchAsset substitutes ${VERSION} into selector and finds the unique
// asset whose name matches by exact equality. Zero or >1 matches → error.
func MatchAsset(release *github.RepositoryRelease, selector, version string) (*github.ReleaseAsset, error) {
	target := strings.ReplaceAll(selector, "${VERSION}", version)
	var matches []*github.ReleaseAsset
	for _, a := range release.Assets {
		if a.GetName() == target {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no asset named %s in release %s", target, release.GetTagName())
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("asset selector %s (resolved %s) matched %d assets in release %s",
			selector, target, len(matches), release.GetTagName())
	}
}

// MatchAssets resolves N selectors against a release in one call. Each
// selector is run through MatchAsset; the per-arch list lands in the
// same order so build can stage them in a deterministic layout. Any
// selector failure aborts the whole resolve so the artifact lands as
// result=error rather than half-populated.
//
// As a defense against same-basename collisions under ${ASSETS}/, the
// resolved-asset basenames are checked for uniqueness; a duplicate
// surfaces as a discover error pointing at both selectors.
func MatchAssets(release *github.RepositoryRelease, selectors []string, version string) ([]*github.ReleaseAsset, error) {
	out := make([]*github.ReleaseAsset, 0, len(selectors))
	seen := make(map[string]int, len(selectors))
	for i, sel := range selectors {
		a, err := MatchAsset(release, sel, version)
		if err != nil {
			return nil, err
		}
		name := a.GetName()
		if prev, ok := seen[name]; ok {
			return nil, fmt.Errorf("asset name collision under ${ASSETS}/: selectors[%d]=%q and selectors[%d]=%q both resolved to %s",
				prev, selectors[prev], i, sel, name)
		}
		seen[name] = i
		out = append(out, a)
	}
	return out, nil
}

// AssetSHA256 extracts the "<hex>" portion of a "sha256:<hex>" digest
// the GitHub API exposes on asset metadata. Returns ok=false if the
// digest is missing or uses an unexpected algorithm — caller should fall
// back to streaming the asset and computing the hash at build time.
func AssetSHA256(a *github.ReleaseAsset) (string, bool) {
	d := a.GetDigest()
	if d == "" {
		return "", false
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return "", false
	}
	return d[len(prefix):], true
}
