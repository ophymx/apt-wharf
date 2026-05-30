package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// hashInput is the exact, ordered, JSON-shape input to JCS for
// build_inputs_hash. Keep this struct in lockstep with cooper-design.md
// §"build_inputs_hash inputs" — the field set is part of the public
// contract.
type hashInput struct {
	FormatRevision int             `json:"format_revision"`
	AssetSHA256s   []*string       `json:"asset_sha256s"`
	BuildPlan      json.RawMessage `json:"build_plan"`
}

// ComputeBuildInputsHash returns "sha256:<hex>" for the artifact-identity
// hash defined in cooper-design.md.
//
//	build_inputs_hash = SHA256(JCS({
//	    "format_revision": tool.format_revision,
//	    "asset_sha256s":   [artifacts[i].assets[*].sha256],
//	    "build_plan":      artifacts[i].build_plan
//	}))
//
// asset_sha256s is the per-asset SHA list in artifact order — the same
// order Assets appears in the plan. A nil entry means "unknown at
// discover time; build will stream + hash" and contributes a JSON null
// to the canonical form. An empty list is the staves / asset-optional
// case.
//
// The hash is deterministic given the same logical inputs across producers
// and across cooper versions sharing a format_revision. Producers in
// other languages reimplement the same algorithm against the published
// corpus.
func ComputeBuildInputsHash(formatRevision int, assetSHA256s []*string, buildPlan BuildPlan) (string, error) {
	bp, err := json.Marshal(buildPlan)
	if err != nil {
		return "", fmt.Errorf("marshal build_plan: %w", err)
	}
	canonical, err := Canonicalize(hashInput{
		FormatRevision: formatRevision,
		AssetSHA256s:   assetSHA256s,
		BuildPlan:      bp,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyBuildInputsHash recomputes Artifact.Deb.BuildInputsHash from the
// rest of the artifact and reports whether it matches. Build calls this
// on every artifact in an incoming plan to defend against tampered or
// stale documents.
//
// Asset-optional (staves) artifacts have an empty/nil Assets slice;
// they pass a nil SHA slice into ComputeBuildInputsHash so the
// canonical form matches what discover computed (JCS distinguishes
// nil from an empty slice — null vs [], different bytes).
func VerifyBuildInputsHash(tool Tool, art Artifact) (ok bool, recomputed string, err error) {
	got, err := ComputeBuildInputsHash(tool.FormatRevision, ArtifactAssetSHAs(art), art.BuildPlan)
	if err != nil {
		return false, "", err
	}
	return got == art.Deb.BuildInputsHash, got, nil
}

// ArtifactAssetSHAs returns the canonical slice of upstream-asset SHAs
// that contributes to an artifact's build_inputs_hash. Named assets
// (Artifact.Assets) come first in their declared order; an
// Artifact.SourceArchive (if set) is appended last. Discover and
// VerifyBuildInputsHash both call through this helper so the slice
// ordering rule lives in one place.
//
// Returns nil — not an empty slice — for asset-optional artifacts
// with no SourceArchive (the staves case). JCS distinguishes null
// from [], so this distinction is load-bearing for hash stability.
func ArtifactAssetSHAs(art Artifact) []*string {
	if len(art.Assets) == 0 && art.SourceArchive == nil {
		return nil
	}
	shas := make([]*string, 0, len(art.Assets)+1)
	for _, a := range art.Assets {
		shas = append(shas, a.SHA256)
	}
	if art.SourceArchive != nil {
		shas = append(shas, art.SourceArchive.SHA256)
	}
	return shas
}
