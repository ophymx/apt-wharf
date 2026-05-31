package plan

import (
	"crypto/sha256"
	"encoding/binary"
)

// DeriveEpoch returns a stable, deterministic source_date_epoch
// derived from arbitrary canonical input bytes. Use this for any
// source kind that lacks a canonical upstream "published_at"
// timestamp — cooper's json_url / xml_url / external kinds, staves
// (local files), chandler (config + fetched keys), and any external
// producer in the same shape.
//
// Algorithm: SHA-256 the input, take the top 63 bits as a positive
// int64, modulo a fixed 10-year window starting at 2020-01-01 UTC.
// Same input bytes → same epoch on every run, every host. Output is
// a "modern-ish" Unix timestamp so `ar tv` listings stay readable
// even though the value is otherwise meaningless.
//
// Callers are responsible for canonicalizing their input — same
// logical inputs must serialize identically, or two equivalent
// recipes will produce different epochs and destabilize
// build_inputs_hash. For URL-shaped sources the canonical form is
// `<url>:<version>`. For content-derived recipes (staves, chandler)
// it's the recipe contents projected through whatever stable
// serialization the tool already uses for build_inputs_hash.
func DeriveEpoch(input []byte) int64 {
	const (
		baseEpoch int64 = 1577836800           // 2020-01-01T00:00:00Z
		spanSecs  int64 = 10 * 365 * 24 * 3600 // ~10 years
	)
	h := sha256.Sum256(input)
	n := int64(binary.BigEndian.Uint64(h[:8]) >> 1) // clamp to positive int64
	return baseEpoch + (n % spanSecs)
}
