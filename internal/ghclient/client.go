// Package ghclient wraps go-github with the small bits every apt-wharf
// tool needs from the GitHub Releases API: an authenticated client
// constructor and a context-driven If-None-Match transport so callers
// can keep API rate-limit usage tight without re-plumbing headers
// through every call site.
package ghclient

import (
	"context"
	"net/http"

	"github.com/google/go-github/v86/github"
)

// etagCtxKey injects per-request If-None-Match into go-github calls
// without having to plumb headers through every API surface.
type etagCtxKey struct{}

// WithEtag returns a derived context that the etag transport will read.
// Passing the empty string returns ctx unchanged so the caller's "no
// prior ETag" path is a no-op.
func WithEtag(ctx context.Context, etag string) context.Context {
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

// New wraps an http.Client with the etag transport and produces a
// go-github *github.Client. Pass token = nil for unauthenticated.
func New(httpClient *http.Client, token []byte) *github.Client {
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
