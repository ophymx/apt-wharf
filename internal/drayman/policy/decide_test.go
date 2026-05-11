package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ophymx/apt-signpost/internal/drayman/backend"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// stubQuerier implements backend.Querier with canned responses keyed
// by query shape.
type stubQuerier struct {
	hashesPresent  map[string]bool              // sha256:<hex> → true
	listByNameArch map[string][]backend.Package // "<name>|<arch>" → packages
	hashErr        error
	listErr        error
}

func (s *stubQuerier) HashExists(_ context.Context, hash string) (bool, error) {
	if s.hashErr != nil {
		return false, s.hashErr
	}
	return s.hashesPresent[hash], nil
}

func (s *stubQuerier) ListByNameArch(_ context.Context, name, arch string) ([]backend.Package, error) {
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

func mkPkg(name, version, arch string) backend.Package {
	return backend.Package{Name: name, Version: version, Architecture: arch}
}

func TestDecide_HashAlreadyInRepo_Skip(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:abc")
	q := &stubQuerier{hashesPresent: map[string]bool{"sha256:abc": true}}
	got, err := Decide(context.Background(), q, p)
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
	q := &stubQuerier{}
	got, err := Decide(context.Background(), q, p)
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
		listByNameArch: map[string][]backend.Package{
			"hugo|amd64": {mkPkg("hugo", "0.140.0", "amd64")},
		},
	}
	got, err := Decide(context.Background(), q, p)
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
		listByNameArch: map[string][]backend.Package{
			"hugo|amd64": {
				mkPkg("hugo", "0.140.0", "amd64"),
				mkPkg("hugo", "0.140.0-1", "amd64"),
				mkPkg("hugo", "0.140.0-3", "amd64"),
				mkPkg("hugo", "0.140.0-10", "amd64"),
			},
		},
	}
	got, err := Decide(context.Background(), q, p)
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
		listByNameArch: map[string][]backend.Package{
			"hugo|amd64": {
				mkPkg("hugo", "0.139.0-5", "amd64"),
				mkPkg("hugo", "0.139.0-7", "amd64"),
			},
		},
	}
	got, err := Decide(context.Background(), q, p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Action != ActionBuild || got[0].Revision != 0 {
		t.Errorf("decision: %+v, want build/0 (different base-V should not contribute)", got[0])
	}
}

func TestDecide_NonIntegerRevisionsExcludedFromMax(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]backend.Package{
			"hugo|amd64": {
				mkPkg("hugo", "0.140.0-1+nmu1", "amd64"),
			},
		},
	}
	got, err := Decide(context.Background(), q, p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Revision != 0 {
		t.Errorf("revision: %d, want 0 (non-integer rev ignored)", got[0].Revision)
	}
}

func TestDecide_EpochInBaseVersion(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "1:0.140.0", "sha256:new")
	q := &stubQuerier{
		listByNameArch: map[string][]backend.Package{
			"hugo|amd64": {
				mkPkg("hugo", "0.140.0-5", "amd64"),   // different epoch — ignored
				mkPkg("hugo", "1:0.140.0-2", "amd64"), // same epoch — contributes
			},
		},
	}
	got, err := Decide(context.Background(), q, p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Revision != 3 {
		t.Errorf("revision: %d, want 3 (max=2 for epoch 1, +1)", got[0].Revision)
	}
}

func TestDecide_SurfacesDiscoverFailedPackages(t *testing.T) {
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
	got, err := Decide(context.Background(), &stubQuerier{}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 decisions (synthetic for broken + real for hugo), got %d", len(got))
	}
	if got[0].PackageName != "broken" || got[0].Action != ActionSkip {
		t.Errorf("first decision: want broken/skip, got %+v", got[0])
	}
	if !strings.Contains(got[0].Reason, "discovery_failed") || !strings.Contains(got[0].Reason, "404") {
		t.Errorf("broken decision reason should carry kind+message, got %q", got[0].Reason)
	}
	if got[1].PackageName != "hugo" || got[1].Action != ActionBuild {
		t.Errorf("second decision: want hugo/build, got %+v", got[1])
	}
}

func TestDecide_BackendErrorBubblesUp(t *testing.T) {
	p := fixturePlan("hugo", "amd64", "0.140.0", "sha256:abc")
	q := &stubQuerier{hashErr: errors.New("backend down")}
	_, err := Decide(context.Background(), q, p)
	if err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Errorf("expected backend-down error, got %v", err)
	}
}
