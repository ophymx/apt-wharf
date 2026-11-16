package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
)

// bare builds a repo entry with no X-Cooper-Build-Inputs-Hash, as a
// prebuilt package imported by `drayman import` has.
func bare(version string) backend.Package {
	return backend.Package{Name: "bigapp", Version: version, Architecture: "amd64", Ref: "ref-" + version}
}

func TestDecideImports(t *testing.T) {
	q := &stubQuerier{listByNameArch: map[string][]backend.Package{
		"bigapp|amd64": {bare("4.1-repack2"), bare("0:4.2-repack3")},
	}}
	debs := []Deb{
		{Path: "a.deb", Name: "bigapp", Version: "4.2-repack3", Arch: "amd64"},      // listed (epoch 0 is implicit)
		{Path: "b.deb", Name: "bigapp", Version: "4.3-repack3", Arch: "amd64"},      // new version
		{Path: "c.deb", Name: "bigapp", Version: "4.1-repack2", Arch: "arm64"},      // listed, but on another arch
		{Path: "d.deb", Name: "bigapp-data", Version: "4.1-repack2", Arch: "amd64"}, // other package
	}
	got, err := DecideImports(context.Background(), q, debs)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		action Action
		code   string
	}{
		{ActionSkip, CodeVersionInRepo},
		{ActionImport, CodeImportPrebuilt},
		{ActionImport, CodeImportPrebuilt},
		{ActionImport, CodeImportPrebuilt},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(got), len(want))
	}
	for i, w := range want {
		d := got[i]
		if d.Action != w.action || d.Code != w.code {
			t.Errorf("%s: got %s/%s, want %s/%s", debs[i].Path, d.Action, d.Code, w.action, w.code)
		}
		if d.PackageName != debs[i].Name || d.Arch != debs[i].Arch || d.PlanVersion != debs[i].Version {
			t.Errorf("%s: decision does not carry the deb's identity: %+v", debs[i].Path, d)
		}
	}
}

func TestDecideImports_QueryError(t *testing.T) {
	q := &stubQuerier{listErr: errors.New("backend down")}
	_, err := DecideImports(context.Background(), q, []Deb{{Name: "bigapp", Version: "1", Arch: "amd64"}})
	if err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Fatalf("want wrapped query error, got %v", err)
	}
}

func TestPlanPrunesAll_HashlessEntriesAreCandidates(t *testing.T) {
	existing := []backend.Package{bare("4.0-repack1"), bare("4.1-repack2"), bare("4.2-repack3")}
	// The hash-scoped policy ignores all of them...
	if got := PlanPrunes(existing, 1, ""); len(got) != 0 {
		t.Fatalf("PlanPrunes must not touch hashless entries, got %v", pruneVersions(got))
	}
	// ...the import policy counts them.
	assertVersions(t, PlanPrunesAll(existing, 2, []string{"4.2-repack3"}), "4.0-repack1")
}

func TestPlanPrunesAll_ScriptBumpSupersedesSameUpstream(t *testing.T) {
	// "4.2-repack2" and "4.2-repack3" share upstream 4.2: the
	// rebuild replaces the older build and doesn't use up a slot.
	existing := []backend.Package{bare("4.1-repack2"), bare("4.2-repack2"), bare("4.2-repack3")}
	got := PlanPrunesAll(existing, 2, []string{"4.2-repack3"})
	assertVersions(t, got, "4.2-repack2")
	if got[0].Code != CodePruneSupersededRevision {
		t.Errorf("code %q, want %q", got[0].Code, CodePruneSupersededRevision)
	}
}

func TestPlanPrunesAll_KeepVersionsProtected(t *testing.T) {
	existing := []backend.Package{bare("4.1-repack2"), bare("4.2-repack3"), bare("4.3-repack3")}
	// 4.1 is beyond retain=1 and would go; naming it keeps it.
	assertVersions(t, PlanPrunesAll(existing, 1, []string{"0:4.1-repack2"}), "4.2-repack3")
	assertVersions(t, PlanPrunesAll(existing, 1, nil), "4.1-repack2", "4.2-repack3")
}

func TestPrunesForAll_ScopedToThePair(t *testing.T) {
	other := backend.Package{Name: "bigapp-data", Version: "3.0-repack1", Architecture: "amd64", Ref: "ref-data"}
	q := &stubQuerier{listByNameArch: map[string][]backend.Package{
		"bigapp|amd64": {bare("4.1-repack2"), bare("4.2-repack3"), other},
	}}
	// Predict: once 4.3 lands, retain=2 drops 4.1 — never the
	// stray entry of another package.
	planned := backend.Package{Name: "bigapp", Version: "4.3-repack3", Architecture: "amd64"}
	got, err := PrunesForAll(context.Background(), q, "bigapp", "amd64", 2, []string{"4.3-repack3"}, planned)
	if err != nil {
		t.Fatal(err)
	}
	assertVersions(t, got, "4.1-repack2")

	if got, err := PrunesForAll(context.Background(), q, "bigapp", "amd64", 0, nil); err != nil || got != nil {
		t.Errorf("retain=0: got %v, %v", got, err)
	}
}
