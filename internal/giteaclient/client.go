// Package giteaclient wraps code.gitea.io/sdk/gitea with the bits apt-wharf
// tools need: constructing a client without the SDK's startup version probe
// (so Wire() stays network-free), an authenticated HTTP transport, and a
// context-driven If-None-Match injector so signpost can keep refresh ticks
// to a single conditional GET against Gitea's release endpoint.
//
// The shape mirrors internal/ghclient for the github_release path; the two
// helpers are deliberately small and parallel rather than fused behind a
// shared interface, since the SDKs disagree on how the per-request ctx is
// plumbed (go-github takes ctx as a method arg; gitea uses Client.SetContext
// for default ctx, then NewRequestWithContext under the hood).
package giteaclient

import (
	"context"
	"net/http"
	"strings"

	"code.gitea.io/sdk/gitea"
)

// etagCtxKey injects per-request If-None-Match into gitea-sdk calls without
// having to thread headers through the SDK surface. Same pattern as the
// ghclient transport.
type etagCtxKey struct{}

// WithEtag returns a derived context that the etag transport will read.
// Passing the empty string returns ctx unchanged so the caller's "no prior
// ETag" path is a no-op.
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

// New constructs a gitea.Client pointed at serverURL with the given token
// (empty string → unauthenticated) and the shared etag transport wrapped
// around httpClient. The SDK's startup version probe is suppressed via
// SetGiteaVersion("") so this call never touches the network.
//
// Each discoverer should hold its own client rather than sharing — the
// Gitea SDK exposes its per-request ctx via Client.SetContext, which is
// mutex-protected but still serial-only for the in-flight request. Sharing
// a client across goroutines that all set their own ctx leads to races.
func New(httpClient *http.Client, serverURL, token string) (*gitea.Client, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	wrapped := *httpClient // copy so the caller's transport isn't mutated
	base := wrapped.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped.Transport = &etagTransport{base: base}

	opts := []gitea.ClientOption{
		gitea.SetGiteaVersion(""), // skip server version probe at construction
		gitea.SetHTTPClient(&wrapped),
	}
	if token != "" {
		opts = append(opts, gitea.SetToken(token))
	}
	return gitea.NewClient(strings.TrimRight(serverURL, "/"), opts...)
}
