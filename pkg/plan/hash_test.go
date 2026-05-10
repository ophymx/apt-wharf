package plan

import (
	"encoding/json"
	"testing"
)

// fixtureBuildPlan is the canonical sample artifact used across the hash
// tests and pinned in the cross-language vector below. Keep stable: any
// edit shifts goldenHash and breaks external producers verifying against
// the published corpus.
func fixtureBuildPlan(t *testing.T) BuildPlan {
	t.Helper()
	nfpm := json.RawMessage(`{
        "name": "hugo",
        "version": "0.140.0",
        "arch": "amd64",
        "platform": "linux",
        "contents": [
            {"src": "${ASSETS}/hugo", "dst": "/usr/bin/hugo", "file_info": {"mode": 493}}
        ]
    }`)
	return BuildPlan{
		SourceDateEpoch: 1715240520,
		Nfpm:            nfpm,
		AuxFiles: map[string]AuxFile{
			"./hugo.service": {ContentB64: "aGVsbG8="},
		},
	}
}

func ptr(s string) *string { return &s }

// TestComputeBuildInputsHash_Golden pins the hash for a known input. This
// is the seed of the cross-language conformance corpus mentioned in
// cooper-design.md §"External producers". Edit only when intentionally
// bumping FormatRevision.
func TestComputeBuildInputsHash_Golden(t *testing.T) {
	got, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), fixtureBuildPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:72c4c3b97cedfe2606b99bfd18943419641387b54ddcfdb51e43154649186ef0"
	if got != want {
		t.Errorf("golden hash drifted:\n got  %s\n want %s", got, want)
	}
}

func TestComputeBuildInputsHash_Deterministic(t *testing.T) {
	bp := fixtureBuildPlan(t)
	a, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("non-deterministic: %s vs %s", a, b)
	}
}

func TestComputeBuildInputsHash_Sensitivity(t *testing.T) {
	base, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), fixtureBuildPlan(t))
	if err != nil {
		t.Fatal(err)
	}

	// Different format_revision → different hash.
	got, err := ComputeBuildInputsHash(2, ptr("sha256:abc123"), fixtureBuildPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Error("format_revision change must change hash")
	}

	// Different asset_sha256 → different hash.
	got, err = ComputeBuildInputsHash(1, ptr("sha256:deadbeef"), fixtureBuildPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Error("asset_sha256 change must change hash")
	}

	// nil asset_sha256 → different hash again.
	got, err = ComputeBuildInputsHash(1, nil, fixtureBuildPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Error("nil asset_sha256 must produce a distinct hash from a set value")
	}

	// Different source_date_epoch → different hash (proves build_plan flows in).
	bp := fixtureBuildPlan(t)
	bp.SourceDateEpoch++
	got, err = ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp)
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Error("source_date_epoch change must change hash")
	}

	// Different aux_files content → different hash.
	bp = fixtureBuildPlan(t)
	bp.AuxFiles["./hugo.service"] = AuxFile{ContentB64: "d29ybGQ="}
	got, err = ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp)
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Error("aux_files content change must change hash")
	}
}

// TestComputeBuildInputsHash_NfpmKeyOrderIrrelevant proves the JCS layer
// folds away accidental key-order differences in the user's doc 2 — same
// logical input, same hash.
func TestComputeBuildInputsHash_NfpmKeyOrderIrrelevant(t *testing.T) {
	bp1 := fixtureBuildPlan(t)
	// Same nfpm content, different key order.
	bp2 := bp1
	bp2.Nfpm = json.RawMessage(`{"arch":"amd64","name":"hugo","version":"0.140.0","platform":"linux","contents":[{"file_info":{"mode":493},"src":"${ASSETS}/hugo","dst":"/usr/bin/hugo"}]}`)

	a, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeBuildInputsHash(1, ptr("sha256:abc123"), bp2)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("key order should not affect hash: %s vs %s", a, b)
	}
}

// TestVerifyBuildInputsHash exercises the build-side defense against
// tampered or stale plans.
func TestVerifyBuildInputsHash(t *testing.T) {
	bp := fixtureBuildPlan(t)
	asset := ptr("sha256:abc123")
	expected, err := ComputeBuildInputsHash(1, asset, bp)
	if err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "cooper", Version: "0.1.0", FormatRevision: 1}
	art := Artifact{
		Arch:      "amd64",
		Asset:     Asset{SHA256: asset},
		Deb:       Deb{BuildInputsHash: expected},
		BuildPlan: bp,
	}
	ok, got, err := VerifyBuildInputsHash(tool, art)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("expected verify=true, got false (recomputed=%s)", got)
	}

	// Mutate build_plan; verify must fail and surface the recomputed hash.
	art.BuildPlan.SourceDateEpoch++
	ok, got, err = VerifyBuildInputsHash(tool, art)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected verify=false after tamper")
	}
	if got == expected {
		t.Errorf("recomputed hash should differ after tamper: %s", got)
	}
}
