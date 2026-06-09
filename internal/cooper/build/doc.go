// Package build implements `cooper build`: consumes a discover-shaped
// JSON plan, fetches and verifies each asset, stages the per-artifact
// nfpm input tree (asset extraction, aux file materialization, mtime
// pinning), invokes nfpm/v2 in-process via Pack to write the .deb, and
// re-emits the plan JSON annotated with the resulting .deb path + sha256.
//
// Build is the second half of cooper's two-phase split per
// cooper-design.md. It defends against tampered or stale input by
// recomputing build_inputs_hash on every artifact before doing any I/O,
// and it pins SOURCE_DATE_EPOCH-derived mtimes plus owner 0/0 to
// guarantee byte-identical .deb output for a given JSON plan.
package build
