package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
)

// hp builds a cooper-built ("hashed") repo entry; the hash is derived
// from the version so tests can refer to it by version.
func hp(version string) backend.Package {
	return backend.Package{
		Name: "hugo", Version: version, Architecture: "amd64",
		BuildInputsHash: "sha256:" + version, Ref: "ref-" + version,
	}
}

func pruneVersions(ps []Prune) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Entry.Version)
	}
	return out
}

func assertVersions(t *testing.T, got []Prune, want ...string) {
	t.Helper()
	gv := pruneVersions(got)
	if strings.Join(gv, ",") != strings.Join(want, ",") {
		t.Fatalf("prune versions: got %v, want %v", gv, want)
	}
}

func TestPlanPrunes_Disabled(t *testing.T) {
	ex := []backend.Package{hp("1.0.0"), hp("2.0.0"), hp("3.0.0")}
	if got := PlanPrunes(ex, 0, ""); got != nil {
		t.Fatalf("retain=0 should be a no-op, got %v", pruneVersions(got))
	}
	if got := PlanPrunes(ex, -1, ""); got != nil {
		t.Fatalf("retain<0 should be a no-op, got %v", pruneVersions(got))
	}
}

func TestPlanPrunes_UnderLimit_NothingPruned(t *testing.T) {
	ex := []backend.Package{hp("1.0.0"), hp("2.0.0")}
	assertVersions(t, PlanPrunes(ex, 3, ""))
}

func TestPlanPrunes_BeyondRetain_OldestFirst(t *testing.T) {
	// Deliberately unsorted input; dpkg ordering, not lexical
	// (0.9.0 < 0.10.0).
	ex := []backend.Package{hp("0.10.0"), hp("0.9.0"), hp("1.0.0"), hp("0.8.0"), hp("2.0.0")}
	got := PlanPrunes(ex, 2, "")
	assertVersions(t, got, "0.8.0", "0.9.0", "0.10.0")
	for _, p := range got {
		if p.Code != CodePruneBeyondRetain {
			t.Errorf("%s: code %q, want %q", p.Entry.Version, p.Code, CodePruneBeyondRetain)
		}
	}
	if got[0].PackageName != "hugo" || got[0].Arch != "amd64" {
		t.Errorf("attribution: %+v", got[0])
	}
}

func TestPlanPrunes_CountsUpstreamNotRevisions(t *testing.T) {
	// Three builds of 1.0.0 must not crowd out 0.9.0 under retain=2:
	// upstream versions are the unit, so only 0.8.0 is beyond retain
	// and the two stale revisions of 1.0.0 are superseded.
	ex := []backend.Package{hp("0.8.0"), hp("0.9.0"), hp("1.0.0"), hp("1.0.0-1"), hp("1.0.0-2")}
	got := PlanPrunes(ex, 2, "")
	assertVersions(t, got, "0.8.0", "1.0.0", "1.0.0-1")
	codes := map[string]string{}
	for _, p := range got {
		codes[p.Entry.Version] = p.Code
	}
	if codes["0.8.0"] != CodePruneBeyondRetain {
		t.Errorf("0.8.0 code %q", codes["0.8.0"])
	}
	if codes["1.0.0"] != CodePruneSupersededRevision || codes["1.0.0-1"] != CodePruneSupersededRevision {
		t.Errorf("revision codes: %v", codes)
	}
	for _, p := range got {
		if p.Code == CodePruneSupersededRevision && !strings.Contains(p.Reason, "1.0.0-2") {
			t.Errorf("superseded reason should name the keeper: %q", p.Reason)
		}
	}
}

func TestPlanPrunes_SupersededRevisionPrunedEvenUnderLimit(t *testing.T) {
	// Only one upstream version, two revisions, retain=5: the stale
	// revision still goes.
	ex := []backend.Package{hp("1.0.0"), hp("1.0.0-1")}
	assertVersions(t, PlanPrunes(ex, 5, ""), "1.0.0")
}

func TestPlanPrunes_ForeignEntriesIgnored(t *testing.T) {
	foreign := backend.Package{Name: "hugo", Version: "5.0.0", Architecture: "amd64", Ref: "manual"}
	ex := []backend.Package{foreign, hp("1.0.0"), hp("2.0.0"), hp("3.0.0")}
	// Foreign 5.0.0 is neither counted (else 3.0.0, 2.0.0 would be the
	// two kept) nor removed.
	assertVersions(t, PlanPrunes(ex, 2, ""), "1.0.0")
}

func TestPlanPrunes_KeepHashProtected(t *testing.T) {
	// A non-integer revision sorts above the integer track that
	// auto-bump produces; the entry this run just imported must
	// survive regardless.
	ex := []backend.Package{hp("1.0.0-nmu1"), hp("1.0.0-1"), hp("1.0.0")}
	got := PlanPrunes(ex, 1, "sha256:1.0.0-1")
	assertVersions(t, got, "1.0.0")
}

func TestPlanPrunes_KeepHashBeyondRetainProtected(t *testing.T) {
	ex := []backend.Package{hp("1.0.0"), hp("2.0.0"), hp("3.0.0")}
	got := PlanPrunes(ex, 1, "sha256:1.0.0")
	assertVersions(t, got, "2.0.0")
}

func TestPlanPrunes_DedupesRepeatedListings(t *testing.T) {
	// reprepro lists an arch:all entry once per binary-<arch> index.
	a := hp("1.0.0")
	a.Architecture = "all"
	b := hp("2.0.0")
	b.Architecture = "all"
	ex := []backend.Package{a, b, a, b}
	got := PlanPrunes(ex, 1, "")
	assertVersions(t, got, "1.0.0")
}

func TestPlanPrunes_NewestOfKeptUpstreamNeverPruned(t *testing.T) {
	ex := []backend.Package{hp("1.0.0"), hp("1.0.0-3")}
	got := PlanPrunes(ex, 1, "")
	assertVersions(t, got, "1.0.0")
	if got[0].Entry.Ref != "ref-1.0.0" {
		t.Errorf("Ref must round-trip to Remove: %+v", got[0].Entry)
	}
}

func TestPrunesFor_MergesExtraAndQueries(t *testing.T) {
	q := &stubQuerier{listByNameArch: map[string][]backend.Package{
		"hugo|amd64": {hp("1.0.0"), hp("2.0.0")},
	}}
	// Predict: once 3.0.0 lands, retain=2 drops 1.0.0 — and the
	// predicted 3.0.0 (no Ref yet) is never itself a candidate.
	planned := backend.Package{Name: "hugo", Version: "3.0.0", Architecture: "amd64", BuildInputsHash: "sha256:new"}
	got, err := PrunesFor(context.Background(), q, "hugo", "amd64", 2, "sha256:new", planned)
	if err != nil {
		t.Fatal(err)
	}
	assertVersions(t, got, "1.0.0")
}

func TestPrunesFor_DisabledSkipsQuery(t *testing.T) {
	q := &stubQuerier{listErr: errors.New("must not be called")}
	got, err := PrunesFor(context.Background(), q, "hugo", "amd64", 0, "")
	if err != nil || got != nil {
		t.Fatalf("retain=0: got %v, %v", got, err)
	}
}

func TestPrunesFor_QueryError(t *testing.T) {
	q := &stubQuerier{listErr: errors.New("backend down")}
	_, err := PrunesFor(context.Background(), q, "hugo", "amd64", 1, "")
	if err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Fatalf("want wrapped query error, got %v", err)
	}
}

func TestPlannedVersion(t *testing.T) {
	if v := PlannedVersion(Decision{PlanVersion: "1.0.0"}); v != "1.0.0" {
		t.Errorf("bare: %q", v)
	}
	if v := PlannedVersion(Decision{PlanVersion: "1.0.0", Revision: 2}); v != "1.0.0-2" {
		t.Errorf("bumped: %q", v)
	}
}
