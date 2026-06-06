package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/sdk/gitea"

	"github.com/ophymx/apt-wharf/internal/giteaclient"
)

// GiteaReleaseDiscoverer probes one source backed by a gitea_release config.
// Mirrors GitHubReleaseDiscoverer; the differences are the SDK call sites
// and how per-request context is plumbed (Gitea's SDK consults the Client's
// default ctx rather than taking one per call).
type GiteaReleaseDiscoverer struct {
	Owner             string
	RepoName          string
	AssetPattern      *regexp.Regexp // anchored ^...$
	IncludePrerelease bool

	Bucket   *Bucket
	BucketID string

	// Client is the per-(server,credential) gitea.Client; constructed via
	// giteaclient.New in production. Each discoverer holds its own client
	// because the Gitea SDK threads per-request ctx through Client.SetContext
	// — sharing across goroutines that all set their own ctx would race.
	Client *gitea.Client
}

// NewGiteaReleaseDiscoverer parses repo into owner/name and anchors the
// asset regex so callers don't have to think about it. The client must
// already be constructed against the desired Gitea server URL.
func NewGiteaReleaseDiscoverer(repo, assetPattern string, includePrerelease bool, bucket *Bucket, bucketID string, client *gitea.Client) (*GiteaReleaseDiscoverer, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("repo %q must be owner/name", repo)
	}
	pat, err := regexp.Compile("^" + assetPattern + "$")
	if err != nil {
		return nil, fmt.Errorf("anchor asset pattern: %w", err)
	}
	return &GiteaReleaseDiscoverer{
		Owner:             owner,
		RepoName:          name,
		AssetPattern:      pat,
		IncludePrerelease: includePrerelease,
		Bucket:            bucket,
		BucketID:          bucketID,
		Client:            client,
	}, nil
}

// Probe fetches the release metadata and selects the matching attachment.
// Mirrors GitHubReleaseDiscoverer.Probe: rate-limit guard up front, then
// either GET /releases/latest or list /releases when prereleases are
// included; the Gitea SDK returns drafts via the same endpoint as github
// so we filter those out ourselves.
func (g *GiteaReleaseDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	allowed, wait := g.Bucket.TryTake()
	if !allowed {
		return nil, fmt.Errorf("rate limit (credential %s) exhausted, next slot in %s",
			g.BucketID, wait.Truncate(0))
	}

	if g.Client == nil {
		return nil, errors.New("gitea client not initialized")
	}

	prevEtag := ""
	if in.Prev != nil {
		prevEtag = in.Prev.APIEtag
	}
	// SetContext is mutex-guarded inside the SDK; since each discoverer
	// holds its own client, there's no cross-source contention on it.
	g.Client.SetContext(giteaclient.WithEtag(ctx, prevEtag))

	if g.IncludePrerelease {
		return g.probeIncludingPrereleases(in.Prev)
	}
	return g.probeLatest(in.Prev)
}

func (g *GiteaReleaseDiscoverer) probeLatest(prev *Probe) (*ProbeResult, error) {
	rel, resp, err := g.Client.GetLatestRelease(g.Owner, g.RepoName)
	if resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotModified {
		return &ProbeResult{Probe: *prev, Unchanged: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetLatestRelease %s/%s: %w", g.Owner, g.RepoName, err)
	}
	res, ok, err := g.tryRelease(rel)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("release %s exposes no asset matching %s",
			rel.TagName, g.AssetPattern)
	}
	return g.finalize(prev, res, giteaEtag(resp)), nil
}

func (g *GiteaReleaseDiscoverer) probeIncludingPrereleases(prev *Probe) (*ProbeResult, error) {
	releases, resp, err := g.Client.ListReleases(g.Owner, g.RepoName, gitea.ListReleasesOptions{
		ListOptions: gitea.ListOptions{PageSize: 10},
	})
	if resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotModified {
		return &ProbeResult{Probe: *prev, Unchanged: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ListReleases %s/%s: %w", g.Owner, g.RepoName, err)
	}
	for _, rel := range releases {
		if rel.IsDraft {
			continue
		}
		res, ok, err := g.tryRelease(rel)
		if err != nil {
			return nil, err
		}
		if ok {
			return g.finalize(prev, res, giteaEtag(resp)), nil
		}
	}
	return nil, fmt.Errorf("no release in %s/%s exposes an asset matching %s",
		g.Owner, g.RepoName, g.AssetPattern)
}

// tryRelease applies the regex to every attachment of one release. Exactly
// one match wins; zero → not viable; multiple → hard error. Gitea
// attachments do not carry a digest field, so AssetDigest is always empty
// — the refresher falls back to streaming the asset to compute SHA256.
func (g *GiteaReleaseDiscoverer) tryRelease(rel *gitea.Release) (*ProbeResult, bool, error) {
	var matches []*gitea.Attachment
	for _, a := range rel.Attachments {
		if g.AssetPattern.MatchString(a.Name) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, false, nil
	case 1:
		a := matches[0]
		return &ProbeResult{
			Probe: Probe{
				URL:        a.DownloadURL,
				Token:      strconv.FormatInt(rel.ID, 10),
				AssetSize:  a.Size,
				ReleaseTag: rel.TagName,
			},
		}, true, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return nil, false, fmt.Errorf("asset regex %s matched %d assets in %s: %s",
			g.AssetPattern, len(matches), rel.TagName, strings.Join(names, ", "))
	}
}

func (g *GiteaReleaseDiscoverer) finalize(prev *Probe, res *ProbeResult, etag string) *ProbeResult {
	res.Probe.APIEtag = etag
	if prev != nil && prev.Token == res.Probe.Token {
		res.Unchanged = true
	}
	return res
}

func giteaEtag(resp *gitea.Response) string {
	if resp == nil || resp.Response == nil {
		return ""
	}
	return resp.Header.Get("ETag")
}
