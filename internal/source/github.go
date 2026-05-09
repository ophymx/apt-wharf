package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v86/github"
)

// etagCtxKey injects per-request If-None-Match into go-github calls without
// having to plumb headers through every API surface.
type etagCtxKey struct{}

// withEtag returns a derived context that the etag transport will read.
func withEtag(ctx context.Context, etag string) context.Context {
	if etag == "" {
		return ctx
	}
	return context.WithValue(ctx, etagCtxKey{}, etag)
}

// etagTransport injects If-None-Match from request context.
type etagTransport struct{ base http.RoundTripper }

func (t *etagTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if v := r.Context().Value(etagCtxKey{}); v != nil {
		r.Header.Set("If-None-Match", v.(string))
	}
	return t.base.RoundTrip(r)
}

// NewGitHubClient wraps an http.Client with the etag transport and produces
// a go-github *github.Client. Pass token = nil for unauthenticated.
func NewGitHubClient(httpClient *http.Client, token []byte) *github.Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	wrapped := *httpClient // copy
	base := wrapped.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped.Transport = &etagTransport{base: base}
	c := github.NewClient(&wrapped)
	if len(token) > 0 {
		c = c.WithAuthToken(string(token))
	}
	return c
}

// GitHubReleaseDiscoverer probes one source backed by a github_release config.
type GitHubReleaseDiscoverer struct {
	Owner             string
	RepoName          string
	AssetPattern      *regexp.Regexp // anchored ^...$
	IncludePrerelease bool

	Bucket   *Bucket
	BucketID string

	// Client is the per-credential go-github client; in production this is
	// constructed via NewGitHubClient. Tests inject a client wired to an
	// httptest server.
	Client *github.Client
}

// NewGitHubReleaseDiscoverer parses repo into owner/name and anchors the
// asset regex so callers don't have to think about it.
func NewGitHubReleaseDiscoverer(repo, assetPattern string, includePrerelease bool, bucket *Bucket, bucketID string, client *github.Client) (*GitHubReleaseDiscoverer, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("repo %q must be owner/name", repo)
	}
	pat, err := regexp.Compile("^" + assetPattern + "$")
	if err != nil {
		return nil, fmt.Errorf("anchor asset pattern: %w", err)
	}
	return &GitHubReleaseDiscoverer{
		Owner:             owner,
		RepoName:          name,
		AssetPattern:      pat,
		IncludePrerelease: includePrerelease,
		Bucket:            bucket,
		BucketID:          bucketID,
		Client:            client,
	}, nil
}

// Probe fetches the release metadata and selects the matching asset.
func (g *GitHubReleaseDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	allowed, wait := g.Bucket.TryTake()
	if !allowed {
		return nil, fmt.Errorf("rate limit (credential %s) exhausted, next slot in %s",
			g.BucketID, wait.Truncate(time.Second))
	}

	// HTTPClient on input is ignored when a Client is set; the discoverer
	// owns transport setup. Tests override Client.BaseURL to point at a
	// local server.
	if g.Client == nil {
		return nil, errors.New("github client not initialized")
	}

	prevEtag := ""
	if in.Prev != nil {
		prevEtag = in.Prev.APIEtag
	}
	apiCtx := withEtag(ctx, prevEtag)

	if g.IncludePrerelease {
		return g.probeIncludingPrereleases(apiCtx, in.Prev)
	}
	return g.probeLatest(apiCtx, in.Prev)
}

func (g *GitHubReleaseDiscoverer) probeLatest(ctx context.Context, prev *Probe) (*ProbeResult, error) {
	rel, resp, err := g.Client.Repositories.GetLatestRelease(ctx, g.Owner, g.RepoName)
	g.applyRateLimit(resp)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
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
			rel.GetTagName(), g.AssetPattern)
	}
	return g.finalize(prev, res, etag(resp)), nil
}

func (g *GitHubReleaseDiscoverer) probeIncludingPrereleases(ctx context.Context, prev *Probe) (*ProbeResult, error) {
	releases, resp, err := g.Client.Repositories.ListReleases(ctx, g.Owner, g.RepoName, &github.ListOptions{PerPage: 10})
	g.applyRateLimit(resp)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &ProbeResult{Probe: *prev, Unchanged: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ListReleases %s/%s: %w", g.Owner, g.RepoName, err)
	}
	for _, rel := range releases {
		res, ok, err := g.tryRelease(rel)
		if err != nil {
			return nil, err
		}
		if ok {
			return g.finalize(prev, res, etag(resp)), nil
		}
	}
	return nil, fmt.Errorf("no release in %s/%s exposes an asset matching %s",
		g.Owner, g.RepoName, g.AssetPattern)
}

// tryRelease applies the regex to all assets of one release. Exactly one
// match wins; zero → not viable; multiple → hard error.
func (g *GitHubReleaseDiscoverer) tryRelease(rel *github.RepositoryRelease) (*ProbeResult, bool, error) {
	var matches []*github.ReleaseAsset
	for _, a := range rel.Assets {
		if g.AssetPattern.MatchString(a.GetName()) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, false, nil
	case 1:
		a := matches[0]
		digest := strings.TrimPrefix(a.GetDigest(), "sha256:")
		if a.GetDigest() != "" && digest == a.GetDigest() {
			// Digest existed but didn't carry the sha256: prefix —
			// treat as missing rather than guessing the algorithm.
			digest = ""
		}
		return &ProbeResult{
			Probe: Probe{
				URL:         a.GetBrowserDownloadURL(),
				Token:       strconv.FormatInt(rel.GetID(), 10),
				AssetSize:   int64(a.GetSize()),
				AssetDigest: digest,
				ReleaseTag:  rel.GetTagName(),
			},
		}, true, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.GetName())
		}
		return nil, false, fmt.Errorf("asset regex %s matched %d assets in %s: %s",
			g.AssetPattern, len(matches), rel.GetTagName(), strings.Join(names, ", "))
	}
}

func (g *GitHubReleaseDiscoverer) finalize(prev *Probe, res *ProbeResult, etag string) *ProbeResult {
	res.Probe.APIEtag = etag
	if prev != nil && prev.Token == res.Probe.Token {
		res.Unchanged = true
	}
	return res
}

func etag(resp *github.Response) string {
	if resp == nil || resp.Response == nil {
		return ""
	}
	return resp.Header.Get("ETag")
}

// applyRateLimit drives the predictive bucket from go-github's parsed Rate.
// We block reactively when Remaining hits 0, mirroring the design's "honor
// GitHub's response headers" rule.
func (g *GitHubReleaseDiscoverer) applyRateLimit(resp *github.Response) {
	if resp == nil {
		return
	}
	// Retry-After takes precedence — it covers secondary rate limits where
	// X-RateLimit-Remaining can still be non-zero.
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		now := time.Now()
		if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
			g.Bucket.BlockUntil(now.Add(time.Duration(secs) * time.Second))
			return
		}
		if t, err := http.ParseTime(ra); err == nil {
			g.Bucket.BlockUntil(t)
			return
		}
	}
	if resp.Rate.Remaining == 0 && !resp.Rate.Reset.IsZero() {
		g.Bucket.BlockUntil(resp.Rate.Reset.Time)
	}
}
