package source

import (
	"crypto/sha256"
	"encoding/binary"
)

// deriveURLEpoch produces a stable timestamp for source kinds that have
// no canonical "published_at" equivalent (json_url, xml_url). github_release
// uses release.PublishedAt directly; URL-based metadata endpoints don't
// reliably expose one, so we hash (url, version) into a deterministic
// second offset from a fixed 2020-01-01 base, capped to a ~10-year window.
//
// The exact value is meaningless — reproducibility just needs the same
// plan to yield the same epoch, and a "modern-ish" timestamp keeps
// `ar tv` listings readable. Same (url, version) → same bytes → same epoch.
func deriveURLEpoch(metadataURL, version string) int64 {
	const (
		baseEpoch int64 = 1577836800           // 2020-01-01T00:00:00Z
		spanSecs  int64 = 10 * 365 * 24 * 3600 // ~10 years
	)
	h := sha256.Sum256([]byte(metadataURL + ":" + version))
	n := int64(binary.BigEndian.Uint64(h[:8]) >> 1) // clamp to positive int64
	return baseEpoch + (n % spanSecs)
}
