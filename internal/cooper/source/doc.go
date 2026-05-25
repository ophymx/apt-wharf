// Package source resolves a cooper sidecar's source.github specification
// to a concrete GitHub release + per-arch asset, using a single API call
// per package (the release is fetched once and the cached Assets slice
// is walked per arch).
//
// The package depends on signpost's internal/source for the
// transport/auth/rate-limit primitives and adds cooper-specific release
// resolution (the three release modes: latest / tag_pattern / tag) and
// asset matching (exact-equality after ${VERSION} substitution).
package source
