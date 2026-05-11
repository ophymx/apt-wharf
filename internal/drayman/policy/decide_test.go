package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ophymx/apt-signpost/internal/drayman/aptly"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// stubQuerier implements policy.Querier with canned responses keyed by
// (repo, query-shape).
type stubQuerier struct {
	hashesPresent  map[string]bool                 // sha256:<hex> → true
	listByNameArch map[string][]aptly.Package      // "<name>|<arch>" → packages
	hashErr        error
	listErr        error
}

func (s *stubQuerier) HashExists(_ context.Context, _, hash string) (bool, error) {
	if s.hashErr != nil {
		return false, s.hashErr
	}
	return s.hashesPresent[hash], nil
}

func (s *stubQuerier) ListByNameArch(_ context.Context, _, name, arch string) ([]aptly.Package, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listByNameArch[name+"|"+arch], nil
}

// fixturePlan returns a one-package one-arch plan whose only artifact's
// hash and bare version we pin so tests can drive decisions.
func fixturePlan(name, arch, bareVersion, hash string) *plan.Plan {
	nfpm, _ := json.Marshal(map[string]any{
		"name":    name,
		"version": bareVersion,
		"arch":    arch,
	})
	return &plan.Plan{
		Packages: []plan.Package{{
			Name:   name,
			Result: plan.ResultOK,
			Artifacts: []plan.Artifact{{
				Arch: arch,
				Deb:  plan.Deb{BuildInputsHash: hash, Filename: name + "_" + bareVersion + "_" + arch + ".deb"},
				BuildPlan: plan.BuildPlan{
					Nfpm: nfpm,
				},
			}},
		}},
	}
}

func TestDecide_HashAlreadyInRepo_Skip(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:abc")
	q := &stubQuerier{
		hashesPresent: map[string]bool{"sha256:abc": true},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 decision, got %d", len(got))
	}
	if got[0].Action != ActionSkip {
		t.Errorf("action: %s, want %s", got[0].Action, ActionSkip)
	}
	if got[0].Revision != 0 {
		t.Errorf("revision: %d, want 0 on skip", got[0].Revision)
	}
}

func TestDecide_FirstBuild_BareVersion(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		hashesPresent: map[string]bool{},
		// No existing entries for hugo amd64.
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Action != ActionBuild || got[0].Revision != 0 {
		t.Errorf("decision: %+v, want build/0", got[0])
	}
}

func TestDecide_BareExists_BumpsToRev1(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]aptly.Package{
			"hugo|amd64": {
				{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k"},
			},
		},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Action != ActionBuild || got[0].Revision != 1 {
		t.Errorf("decision: %+v, want build/1", got[0])
	}
}

func TestDecide_ExistingRevisions_FindsMaxAndBumps(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]aptly.Package{
			"hugo|amd64": {
				{Package: "hugo", Version: "0.140.0", Architecture: "amd64", Key: "k0"},
				{Package: "hugo", Version: "0.140.0-1", Architecture: "amd64", Key: "k1"},
				{Package: "hugo", Version: "0.140.0-3", Architecture: "amd64", Key: "k3"},
				{Package: "hugo", Version: "0.140.0-10", Architecture: "amd64", Key: "k10"},
			},
		},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Revision != 11 {
		t.Errorf("revision: %d, want 11 (max=10, +1)", got[0].Revision)
	}
}

func TestDecide_DifferentBaseVersionIgnored(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]aptly.Package{
			"hugo|amd64": {
				// Existing entries for a DIFFERENT base version — must not
				// poison the max-revision computation for 0.140.0.
				{Package: "hugo", Version: "0.139.0-5", Architecture: "amd64", Key: "k1"},
				{Package: "hugo", Version: "0.139.0-7", Architecture: "amd64", Key: "k2"},
			},
		},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	// No matching base-V → publish at bare.
	if got[0].Action != ActionBuild || got[0].Revision != 0 {
		t.Errorf("decision: %+v, want build/0 (different base-V should not contribute)", got[0])
	}
}

func TestDecide_NonIntegerRevisionsExcludedFromMax(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]aptly.Package{
			"hugo|amd64": {
				// Recipe-baked oddity that cooper-only orchestrators
				// wouldn't produce. Drayman should not crash and should
				// not include this in max-int-revision computation.
				{Package: "hugo", Version: "0.140.0-1+nmu1", Architecture: "amd64", Key: "k1"},
			},
		},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	// "1+nmu1" doesn't parse as int → maxRev stays -1 → publish bare.
	if got[0].Revision != 0 {
		t.Errorf("revision: %d, want 0 (non-integer rev ignored)", got[0].Revision)
	}
}

func TestDecide_EpochInBaseVersion(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "1:0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]aptly.Package{
			"hugo|amd64": {
				// Different epoch — must not contribute (different
				// version line per Debian).
				{Package: "hugo", Version: "0.140.0-5", Architecture: "amd64", Key: "k1"},
				// Same epoch + base-V → contributes.
				{Package: "hugo", Version: "1:0.140.0-2", Architecture: "amd64", Key: "k2"},
			},
		},
	}
	got, err := Decide(context.Background(), q, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Revision != 3 {
		t.Errorf("revision: %d, want 3 (max=2 for epoch 1, +1)", got[0].Revision)
	}
}

func TestDecide_SkipsArtifactsInErrorPackages(t *testing.T) {
	p := &plan.Plan{
		Packages: []plan.Package{
			{Name: "broken", Result: plan.ResultError, Error: &plan.Error{Kind: "discovery_failed", Message: "404"}},
			{
				Name:   "hugo",
				Result: plan.ResultOK,
				Artifacts: []plan.Artifact{{
					Arch: "amd64",
					Deb:  plan.Deb{BuildInputsHash: "sha256:abc", Filename: "hugo_0.140.0_amd64.deb"},
					BuildPlan: plan.BuildPlan{
						Nfpm: json.RawMessage(`{"name":"hugo","version":"0.140.0"}`),
					},
				}},
			},
		},
	}
	got, err := Decide(context.Background(), &stubQuerier{}, "repo", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("want 1 decision (broken package skipped), got %d", len(got))
	}
	if got[0].PackageName != "hugo" {
		t.Errorf("unexpected decision target: %s", got[0].PackageName)
	}
}

func TestDecide_AptlyErrorBubblesUp(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:abc")
	q := &stubQuerier{hashErr: errors.New("aptly down")}
	_, err := Decide(context.Background(), q, "repo", p)
	if err == nil || !errorsContains(err, "aptly down") {
		t.Errorf("expected aptly-down error, got %v", err)
	}
}

func errorsContains(err error, want string) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), want)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
