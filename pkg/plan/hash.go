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
	AssetSHA256    *string         `json:"asset_sha256"`
	BuildPlan      json.RawMessage `json:"build_plan"`
}

// ComputeBuildInputsHash returns "sha256:<hex>" for the artifact-identity
// hash defined in cooper-design.md.
//
//	build_inputs_hash = SHA256(JCS({
//	    "format_revision": tool.format_revision,
//	    "asset_sha256":    artifacts[i].asset.sha256,
//	    "build_plan":      artifacts[i].build_plan
//	}))
//
// The hash is deterministic given the same logical inputs across producers
// and across cooper versions sharing a format_revision. Producers in
// other languages reimplement the same algorithm against the published
// corpus.
func ComputeBuildInputsHash(formatRevision int, assetSHA256 *string, buildPlan BuildPlan) (string, error) {
	bp, err := json.Marshal(buildPlan)
	if err != nil {
		return "", fmt.Errorf("marshal build_plan: %w", err)
	}
	canonical, err := Canonicalize(hashInput{
		FormatRevision: formatRevision,
		AssetSHA256:    assetSHA256,
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
func VerifyBuildInputsHash(tool Tool, art Artifact) (ok bool, recomputed string, err error) {
	got, err := ComputeBuildInputsHash(tool.FormatRevision, art.Asset.SHA256, art.BuildPlan)
	if err != nil {
		return false, "", err
	}
	return got == art.Deb.BuildInputsHash, got, nil
}
