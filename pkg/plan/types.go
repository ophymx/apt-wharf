package plan

import "encoding/json"

// Plan is the top-level document produced by `cooper discover` (or any
// external producer) and consumed by `cooper build`.
type Plan struct {
	SchemaVersion int       `json:"schema_version"`
	Tool          Tool      `json:"tool"`
	DiscoveredAt  string    `json:"discovered_at"` // RFC 3339 UTC
	Packages      []Package `json:"packages"`
}

// Tool identifies the producer of a plan. External producers MAY set Name
// to their own tool name; FormatRevision MUST point at an actual cooper
// release whose hash algorithm they implement.
type Tool struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	FormatRevision int    `json:"format_revision"`
}

// Package is one logical Debian package — typically multi-arch.
//
// On Result == ResultOK, Source and Artifacts are populated and Error is nil.
// On Result == ResultError, Error is populated and the rest is nil.
type Package struct {
	Name      string     `json:"name"`
	Result    string     `json:"result"`
	Source    *Source    `json:"source,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Error     *Error     `json:"error,omitempty"`
}

// Source is provenance for a successfully discovered package. Excluded
// from build_inputs_hash by design — pure provenance.
//
// Field population depends on Kind:
//   - "github_release": Repo, ReleaseID, ReleaseTag, ReleasePublishedAt.
//   - "json_url":       URL, Token (the extracted version), nothing else.
//
// All non-Kind fields use omitempty so each kind's record stays focused
// on its native provenance.
type Source struct {
	Kind               string `json:"kind"`
	Repo               string `json:"repo,omitempty"`
	ReleaseID          int64  `json:"release_id,omitempty"`
	ReleaseTag         string `json:"release_tag,omitempty"`
	ReleasePublishedAt string `json:"release_published_at,omitempty"` // RFC 3339 UTC
	URL                string `json:"url,omitempty"`
	Token              string `json:"token,omitempty"`
}

// Artifact is one (package, arch) tuple — the unit at which cooper builds
// .debs and at which orchestrators dedup against the target apt repo.
//
// Result/Error are populated only by `cooper build` when an artifact's
// build fails — discover never sets them. Empty Result is "ok"; the
// failure-isolation rule in cooper-design.md §"Phases · Build" lets
// sibling artifacts in the same package finish even when one errors.
type Artifact struct {
	Arch      string    `json:"arch"`
	Asset     Asset     `json:"asset"`
	Deb       Deb       `json:"deb"`
	BuildPlan BuildPlan `json:"build_plan"`
	Result    string    `json:"result,omitempty"`
	Error     *Error    `json:"error,omitempty"`
}

// Asset describes the upstream binary cooper will fetch.
//
// SHA256 is "sha256:<hex>" or nil. SHA256Source is "github_api" /
// "head_request" / nil. nil means build will stream + hash and record the
// value in the re-emitted JSON; the orchestrator should treat such
// artifacts as "re-import unconditionally" because dedup needs the SHA in
// build_inputs_hash.
type Asset struct {
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	Size         int64   `json:"size"`
	SHA256       *string `json:"sha256"`
	SHA256Source *string `json:"sha256_source"`
}

// Deb is the per-artifact .deb identity. Path/SHA256 are nil in discover
// output and populated in the annotated JSON `cooper build` re-emits.
type Deb struct {
	Filename        string  `json:"filename"`
	BuildInputsHash string  `json:"build_inputs_hash"`
	Path            *string `json:"path"`
	SHA256          *string `json:"sha256"`
}

// BuildPlan carries everything build needs to produce the .deb. The Nfpm
// subtree is doc 2 of the per-package multi-doc YAML, with ${VERSION} and
// ${ARCH} substituted to literals; ${ASSETS} is left symbolic for nfpm to
// expand at build time.
//
// Nfpm is held as json.RawMessage so it round-trips byte-for-byte through
// re-emission. Build serializes it back to YAML for nfpm to read.
type BuildPlan struct {
	SourceDateEpoch int64              `json:"source_date_epoch"`
	Nfpm            json.RawMessage    `json:"nfpm"`
	AuxFiles        map[string]AuxFile `json:"aux_files"`
}

// AuxFile is one inlined source-tree file referenced by doc 2 (literal
// path, glob match, recursively-staged directory entry, or rendered .tmpl
// output). The map key is the source-relative path the user wrote.
type AuxFile struct {
	ContentB64 string `json:"content_b64"`
}

// Error is the body of a Package whose Result is ResultError.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}
