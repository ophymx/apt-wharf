package source

import (
	"fmt"
	"net/http"
	"strings"

	"code.gitea.io/sdk/gitea"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
)

// ResolveGiteaRelease fetches the Gitea release identified by gt. The three
// release modes from the shared Release union map identically to the
// github_release path:
//
//   - Latest=true, IncludePrerelease=false → GET /releases/latest
//   - Latest=true, IncludePrerelease=true  → list, take the first non-draft
//   - TagPattern set                       → list, take the first non-draft
//     whose tag matches (and skip prereleases unless IncludePrerelease)
//   - Tag set                              → GET /releases/tags/<tag>
//
// The client must already be constructed against gt.Server; the caller (the
// cooper cmd) caches one client per (server, token) pair so multiple
// gitea-sourced packages on the same instance share the connection pool.
func ResolveGiteaRelease(client *gitea.Client, gt *config.GiteaSource) (*gitea.Release, error) {
	owner, name, ok := strings.Cut(gt.Repo, "/")
	if !ok {
		return nil, fmt.Errorf("invalid repo %q", gt.Repo)
	}

	switch {
	case gt.Release.Tag != "":
		rel, resp, err := client.GetReleaseByTag(owner, name, gt.Release.Tag)
		if err != nil {
			return nil, fmt.Errorf("GetReleaseByTag %s/%s %s: %w", owner, name, gt.Release.Tag, err)
		}
		if resp != nil && resp.Response != nil && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GetReleaseByTag %s/%s %s: status %s", owner, name, gt.Release.Tag, resp.Status)
		}
		return rel, nil

	case gt.Release.Latest && !gt.IncludePrerelease:
		rel, resp, err := client.GetLatestRelease(owner, name)
		if err != nil {
			return nil, fmt.Errorf("GetLatestRelease %s/%s: %w", owner, name, err)
		}
		if resp != nil && resp.Response != nil && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GetLatestRelease %s/%s: status %s", owner, name, resp.Status)
		}
		return rel, nil

	case gt.Release.Latest && gt.IncludePrerelease:
		return firstMatchingGitea(client, owner, name, true, nil)

	case gt.Release.TagPattern != "":
		re := gt.Release.CompiledTagPattern()
		if re == nil {
			return nil, fmt.Errorf("tag_pattern not compiled (config bug?)")
		}
		return firstMatchingGitea(client, owner, name, gt.IncludePrerelease, func(tag string) bool {
			return re.MatchString(tag)
		})

	default:
		return nil, fmt.Errorf("invalid release union (validate should have caught this)")
	}
}

// firstMatchingGitea lists releases (newest first per Gitea API ordering),
// skips drafts, optionally skips prereleases, and applies an optional tag
// filter. Mirrors the github firstMatching: page size 30 covers the common
// case; recipes stuck on a far-back release should pin a specific tag.
func firstMatchingGitea(
	client *gitea.Client,
	owner, name string,
	includePrerelease bool,
	tagFilter func(string) bool,
) (*gitea.Release, error) {
	releases, resp, err := client.ListReleases(owner, name, gitea.ListReleasesOptions{
		ListOptions: gitea.ListOptions{PageSize: 30},
	})
	if err != nil {
		return nil, fmt.Errorf("ListReleases %s/%s: %w", owner, name, err)
	}
	if resp != nil && resp.Response != nil && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListReleases %s/%s: status %s", owner, name, resp.Status)
	}
	for _, r := range releases {
		if r.IsDraft {
			continue
		}
		if !includePrerelease && r.IsPrerelease {
			continue
		}
		if tagFilter != nil && !tagFilter(r.TagName) {
			continue
		}
		return r, nil
	}
	return nil, fmt.Errorf("no release in %s/%s matched the configured filters", owner, name)
}

// MatchGiteaAsset substitutes ${VERSION} into selector and finds the unique
// attachment whose name matches by exact equality. Zero or >1 matches → error.
func MatchGiteaAsset(release *gitea.Release, selector, version string) (*gitea.Attachment, error) {
	target := strings.ReplaceAll(selector, "${VERSION}", version)
	var matches []*gitea.Attachment
	for _, a := range release.Attachments {
		if a.Name == target {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no asset named %s in release %s", target, release.TagName)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("asset selector %s (resolved %s) matched %d assets in release %s",
			selector, target, len(matches), release.TagName)
	}
}

// MatchGiteaAssets resolves N selectors against a release in one call.
// Mirrors MatchAssets: any selector failure aborts the whole resolve;
// duplicate basenames are rejected so ${ASSETS}/ stays collision-free.
func MatchGiteaAssets(release *gitea.Release, selectors []string, version string) ([]*gitea.Attachment, error) {
	out := make([]*gitea.Attachment, 0, len(selectors))
	seen := make(map[string]int, len(selectors))
	for i, sel := range selectors {
		a, err := MatchGiteaAsset(release, sel, version)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[a.Name]; ok {
			return nil, fmt.Errorf("asset name collision under ${ASSETS}/: selectors[%d]=%q and selectors[%d]=%q both resolved to %s",
				prev, selectors[prev], i, sel, a.Name)
		}
		seen[a.Name] = i
		out = append(out, a)
	}
	return out, nil
}
