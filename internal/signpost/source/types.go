// Package source implements upstream-discovery — for MVP, GitHub releases.
//
// Discoverer.Probe encapsulates the "what URL, has it changed?" question.
// Hashing, control extraction, and state persistence live elsewhere.
package source

import (
	"context"
	"net/http"
)

// Probe is the token + URL pair the refresher carries between ticks for a
// single source. Tokens are opaque to the discoverer interface — the refresh
// loop only checks current.Token == prev.Token to decide whether to skip.
type Probe struct {
	URL   string
	Token string

	// Optional metadata exposed when the discoverer can supply it cheaply.
	// AssetSize is from upstream API responses; AssetDigest is the
	// "sha256:HEX" form when available (used to skip the streaming hash).
	AssetSize   int64
	AssetDigest string
	ReleaseTag  string
	APIEtag     string
}

// ProbeInput bundles per-tick caller context.
type ProbeInput struct {
	Prev *Probe // nil on cold start

	// HTTPClient is the client to use for outbound calls. Caller owns
	// timeouts on it.
	HTTPClient *http.Client
}

// ProbeResult carries the new probe plus a flag to short-circuit the tick.
type ProbeResult struct {
	Probe     Probe
	Unchanged bool // true when prev.Token == new.Token (skip fetch)
}

// Discoverer is the interface every source type implements.
type Discoverer interface {
	Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error)
}
