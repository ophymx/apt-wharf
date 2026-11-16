package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/drayman/audit"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/internal/drayman/policy"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// stubBackend is an in-memory backend.Backend + backend.Querier for
// reconcile-flow tests. State is set via fields and inspected via
// Imports/PublishCount/HashExistsCalls after each run. Set
// {Hash,List,Import,Publish}Err to inject failures at each step.
type stubBackend struct {
	// HashExists responses keyed by hash. Missing keys return false.
	hashes map[string]bool
	// ListByNameArch responses keyed by "<name>|<arch>".
	listings map[string][]backend.Package

	hashErr error
	listErr error
	// listErrAfterImport fails ListByNameArch only once an Import has
	// happened: Decide's listing succeeds, the retain re-list doesn't.
	listErrAfterImport error
	importErr          map[string]error // keyed by basename; missing key → nil
	removeErr          map[string]error // keyed by version; missing key → nil
	publishErr         error
	cleanupErr         error

	// landOnImport makes Import update listings; silentlyDropped
	// names basenames it accepts without listing (reprepro declining
	// a superseded version exits 0).
	landOnImport    bool
	silentlyDropped map[string]bool

	imports      []string          // deb paths in call order
	importCalls  int               // Import invocations (a batch counts once)
	removes      []backend.Package // entries passed to Remove, in call order
	publishCount int
	cleanupCount int
	ops          []string // "publish" / "cleanup" in call order
}

func newStubBackend() *stubBackend {
	return &stubBackend{
		hashes:    map[string]bool{},
		listings:  map[string][]backend.Package{},
		importErr: map[string]error{},
		removeErr: map[string]error{},
	}
}

func (s *stubBackend) HashExists(_ context.Context, h string) (bool, error) {
	if s.hashErr != nil {
		return false, s.hashErr
	}
	return s.hashes[h], nil
}

func (s *stubBackend) ListByNameArch(_ context.Context, name, arch string) ([]backend.Package, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listErrAfterImport != nil && len(s.imports) > 0 {
		return nil, s.listErrAfterImport
	}
	return s.listings[name+"|"+arch], nil
}

// ListByName flattens every listing whose entries carry name, in
// sorted key order so output is deterministic.
func (s *stubBackend) ListByName(_ context.Context, name string) ([]backend.Package, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	keys := make([]string, 0, len(s.listings))
	for k := range s.listings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []backend.Package
	for _, k := range keys {
		for _, p := range s.listings[k] {
			if p.Name == name {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Import records every path, then fails the whole call if any of
// them has an injected error (a batch does not land cleanly). With
// landOnImport set, each path's (name, version, arch) — parsed from
// the conventional <name>_<version>_<arch>.deb basename — is added to
// the listings, as a real backend's re-list would show.
func (s *stubBackend) Import(_ context.Context, debPaths ...string) error {
	s.imports = append(s.imports, debPaths...)
	s.importCalls++
	for _, debPath := range debPaths {
		if err, ok := s.importErr[filepath.Base(debPath)]; ok {
			return err
		}
	}
	if s.landOnImport {
		for _, debPath := range debPaths {
			parts := strings.Split(strings.TrimSuffix(filepath.Base(debPath), ".deb"), "_")
			if len(parts) != 3 || s.silentlyDropped[filepath.Base(debPath)] {
				continue
			}
			key := parts[0] + "|" + parts[2]
			s.listings[key] = append(s.listings[key], backend.Package{
				Name: parts[0], Version: parts[1], Architecture: parts[2], Ref: "imported-" + parts[1],
			})
		}
	}
	return nil
}

func (s *stubBackend) Remove(_ context.Context, pkg backend.Package) error {
	s.removes = append(s.removes, pkg)
	if err, ok := s.removeErr[pkg.Version]; ok {
		return err
	}
	return nil
}

func (s *stubBackend) Publish(_ context.Context) error {
	s.publishCount++
	s.ops = append(s.ops, "publish")
	return s.publishErr
}

func (s *stubBackend) Cleanup(_ context.Context) error {
	s.cleanupCount++
	s.ops = append(s.ops, "cleanup")
	return s.cleanupErr
}

// makePlan builds a discover-style plan with one artifact per (name, arch).
// nfpm version is baked into each artifact's BuildPlan.Nfpm so policy.Decide
// can extract a bare version.
func makePlan(t *testing.T, packages ...stubPkg) *plan.Plan {
	t.Helper()
	p := &plan.Plan{
		SchemaVersion: plan.SchemaVersion,
		Tool:          plan.Tool{Name: "test", FormatRevision: plan.FormatRevision},
		DiscoveredAt:  "2026-05-10T00:00:00Z",
	}
	for _, sp := range packages {
		pkg := plan.Package{Name: sp.name, Result: plan.ResultOK}
		if sp.discoverErr != "" {
			pkg.Result = plan.ResultError
			pkg.Error = &plan.Error{Kind: "discover", Message: sp.discoverErr}
			p.Packages = append(p.Packages, pkg)
			continue
		}
		for _, arch := range sp.arches {
			nfpm := json.RawMessage(fmt.Sprintf(
				`{"name":%q,"version":%q,"arch":%q}`, sp.name, sp.version, arch))
			pkg.Artifacts = append(pkg.Artifacts, plan.Artifact{
				Arch: arch,
				Assets: []plan.Asset{{
					Name: fmt.Sprintf("%s-%s.tar.gz", sp.name, arch),
					URL:  "https://example.invalid/" + sp.name,
				}},
				Deb: plan.Deb{
					Filename:        fmt.Sprintf("%s_%s_%s.deb", sp.name, sp.version, arch),
					BuildInputsHash: fmt.Sprintf("sha256:%s-%s-%s", sp.name, sp.version, arch),
				},
				BuildPlan: plan.BuildPlan{
					SourceDateEpoch: 1700000000,
					Nfpm:            nfpm,
				},
			})
		}
		p.Packages = append(p.Packages, pkg)
	}
	return p
}

type stubPkg struct {
	name        string
	version     string
	arches      []string
	discoverErr string // non-empty → emit Result=error package
}

// stubCooperBuildScript stamps Result/Path/Error onto each artifact
// of the filtered plan and returns it as the annotated plan. The
// per-artifact behavior is keyed by "<package>|<arch>".
//
// missing key → success with synthetic Path under outDir
// entry with err → annotate as ResultError
type stubCooperBuildScript struct {
	failures map[string]string // "<pkg>|<arch>" → error message
	// returnErr, when set, makes the stub return (nil, err) — modeling
	// cooper exiting before emitting a valid plan.
	returnErr error
	// recorded calls
	calls []stubCooperBuildCall
}

type stubCooperBuildCall struct {
	Revision   int
	OutDir     string
	WorkDir    string
	PlanArches []string // "<pkg>|<arch>" for every artifact in the filtered plan
}

func (s *stubCooperBuildScript) Fn() cooperBuildFn {
	return func(_ context.Context, p *plan.Plan, outDir, workDir string, rev int) (*plan.Plan, error) {
		call := stubCooperBuildCall{Revision: rev, OutDir: outDir, WorkDir: workDir}
		for _, pkg := range p.Packages {
			for _, a := range pkg.Artifacts {
				call.PlanArches = append(call.PlanArches, pkg.Name+"|"+a.Arch)
			}
		}
		s.calls = append(s.calls, call)
		if s.returnErr != nil {
			return nil, s.returnErr
		}
		annotated := *p
		annotated.Packages = nil
		for _, pkg := range p.Packages {
			newPkg := pkg
			newPkg.Artifacts = nil
			for _, a := range pkg.Artifacts {
				newArt := a
				if msg, fail := s.failures[pkg.Name+"|"+a.Arch]; fail {
					newArt.Result = plan.ResultError
					newArt.Error = &plan.Error{Kind: "build", Message: msg}
				} else {
					newArt.Result = plan.ResultOK
					path := filepath.Join(outDir, a.Deb.Filename)
					newArt.Deb.Path = &path
				}
				newPkg.Artifacts = append(newPkg.Artifacts, newArt)
			}
			annotated.Packages = append(annotated.Packages, newPkg)
		}
		return &annotated, nil
	}
}

// runOpts is a small helper that fills in the boring bits of
// reconcileOpts so individual test cases just override what matters.
func runOpts(be backend.Backend, p *plan.Plan, build cooperBuildFn, stdout, stderr *bytes.Buffer) reconcileOpts {
	return reconcileOpts{
		Backend:     be,
		BackendKind: "stub",
		Plan:        p,
		CooperBuild: build,
		OutDir:      "/tmp/test-out",
		WorkDir:     "/tmp/test-work",
		Stdout:      stdout,
		Stderr:      stderr,
	}
}

// --- tests ------------------------------------------------------------------

func TestRunReconcile_HappyPath(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if len(be.imports) != 1 {
		t.Fatalf("imports=%v, want 1", be.imports)
	}
	if !strings.HasSuffix(be.imports[0], "hugo_0.140.0_amd64.deb") {
		t.Errorf("imported %q, want hugo .deb", be.imports[0])
	}
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1", be.publishCount)
	}
	if len(script.calls) != 1 || script.calls[0].Revision != 0 {
		t.Errorf("cooper calls=%+v, want one call at rev 0", script.calls)
	}
	if !strings.Contains(stdout.String(), "imported: hugo_0.140.0_amd64.deb") {
		t.Errorf("stdout missing import line: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "publish updated") {
		t.Errorf("stdout missing publish line: %q", stdout.String())
	}
}

func TestRunReconcile_AllSkipNoPublish(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	// pre-populate the hash so policy says skip
	be.hashes[p.Packages[0].Artifacts[0].Deb.BuildInputsHash] = true
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if len(script.calls) != 0 {
		t.Errorf("cooper called %d times, want 0", len(script.calls))
	}
	if len(be.imports) != 0 {
		t.Errorf("imports=%v, want 0", be.imports)
	}
	if be.publishCount != 0 {
		t.Errorf("publishCount=%d, want 0 (anyImported gate)", be.publishCount)
	}
	if !strings.Contains(stdout.String(), "nothing to import") {
		t.Errorf("stdout missing all-skipped line: %q", stdout.String())
	}
}

func TestRunReconcile_DryRun(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.DryRun = true
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if len(script.calls) != 0 {
		t.Errorf("cooper called under dry-run: %+v", script.calls)
	}
	if len(be.imports) != 0 || be.publishCount != 0 {
		t.Errorf("dry-run wrote: imports=%v publish=%d", be.imports, be.publishCount)
	}
	if !strings.Contains(stdout.String(), "dry-run") {
		t.Errorf("stdout missing dry-run marker: %q", stdout.String())
	}
	// Decisions still printed during dry-run.
	if !strings.Contains(stdout.String(), "hugo\tamd64\tbuild") {
		t.Errorf("stdout missing decision row: %q", stdout.String())
	}
}

func TestRunReconcile_MixedBuildFailure(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t,
		stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64", "arm64"}},
	)
	script := &stubCooperBuildScript{
		failures: map[string]string{"hugo|arm64": "nfpm exploded"},
	}
	var stdout, stderr bytes.Buffer

	err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	if err == nil {
		t.Fatal("runReconcile: want error, got nil")
	}
	// amd64 succeeded → still imported and published.
	if len(be.imports) != 1 || !strings.Contains(be.imports[0], "amd64") {
		t.Errorf("imports=%v, want one amd64 import", be.imports)
	}
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1 (anyImported true even with failures)", be.publishCount)
	}
	if !strings.Contains(stderr.String(), "build failed: hugo arm64: nfpm exploded") {
		t.Errorf("stderr missing build-failed line: %q", stderr.String())
	}
}

func TestRunReconcile_CooperSubprocessError(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{returnErr: errors.New("cooper crashed")}
	var stdout, stderr bytes.Buffer

	err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	if err == nil || !strings.Contains(err.Error(), "cooper crashed") {
		t.Fatalf("runReconcile err=%v, want wrapping 'cooper crashed'", err)
	}
	if len(be.imports) != 0 || be.publishCount != 0 {
		t.Errorf("cooper error should have aborted writes: imports=%v publish=%d", be.imports, be.publishCount)
	}
}

func TestRunReconcile_ImportErrorStillPublishesOtherSuccess(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64", "arm64"}})
	be.importErr["hugo_0.140.0_amd64.deb"] = errors.New("403 forbidden")
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	if err == nil {
		t.Fatal("runReconcile: want error, got nil")
	}
	// Both Import calls attempted; arm64 succeeded → Publish runs.
	if len(be.imports) != 2 {
		t.Errorf("imports=%v, want 2 attempts", be.imports)
	}
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1", be.publishCount)
	}
	if !strings.Contains(stderr.String(), "import failed: hugo_0.140.0_amd64.deb") {
		t.Errorf("stderr missing import-failed line: %q", stderr.String())
	}
}

func TestRunReconcile_PublishError(t *testing.T) {
	be := newStubBackend()
	be.publishErr = errors.New("aptly snapshot locked")
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	if err == nil || !strings.Contains(err.Error(), "aptly snapshot locked") {
		t.Fatalf("runReconcile err=%v, want wrapping publish error", err)
	}
	if len(be.imports) != 1 {
		t.Errorf("imports=%v, want 1 (publish ran after import)", be.imports)
	}
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1 attempted", be.publishCount)
	}
}

func TestRunReconcile_RevisionGrouping(t *testing.T) {
	// hugo: brand new (rev 0). nfpm: existing repo entries at same
	// base-version drive auto-bump → rev 1.
	be := newStubBackend()
	p := makePlan(t,
		stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}},
		stubPkg{name: "nfpm", version: "2.40.0", arches: []string{"amd64"}},
	)
	be.listings["nfpm|amd64"] = []backend.Package{
		{Name: "nfpm", Version: "2.40.0", Architecture: "amd64", BuildInputsHash: "sha256:old-nfpm"},
	}
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if len(script.calls) != 2 {
		t.Fatalf("cooper calls=%d, want 2 (one per revision bucket)", len(script.calls))
	}
	// sortedRevisions returns ascending; rev 0 (hugo) first, rev 1 (nfpm) second.
	if script.calls[0].Revision != 0 || script.calls[1].Revision != 1 {
		t.Errorf("revisions=[%d,%d], want [0,1]", script.calls[0].Revision, script.calls[1].Revision)
	}
	if got := script.calls[0].PlanArches; len(got) != 1 || got[0] != "hugo|amd64" {
		t.Errorf("rev 0 group=%v, want [hugo|amd64]", got)
	}
	if got := script.calls[1].PlanArches; len(got) != 1 || got[0] != "nfpm|amd64" {
		t.Errorf("rev 1 group=%v, want [nfpm|amd64]", got)
	}
}

func TestRunReconcile_AuditLogInvariants(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	var logBuf bytes.Buffer
	logger := audit.NewWriter(&logBuf)

	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.AuditLog = logger
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}

	lines := strings.Split(strings.TrimRight(logBuf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("audit lines=%d, want 3 (decision, import, publish):\n%s", len(lines), logBuf.String())
	}
	types := []string{}
	for _, line := range lines {
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit JSON %q: %v", line, err)
		}
		types = append(types, e.Type)
	}
	wantTypes := []string{audit.TypeDecision, audit.TypeImport, audit.TypePublish}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Errorf("audit[%d].type=%s, want %s", i, types[i], want)
		}
	}
}

func TestRunReconcile_DiscoverFailedNoBuild(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t,
		stubPkg{name: "broken", discoverErr: "upstream 404"},
		stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}},
	)
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	// One cooper invocation (hugo); broken is a skip decision only.
	if len(script.calls) != 1 {
		t.Fatalf("cooper calls=%d, want 1", len(script.calls))
	}
	for _, slot := range script.calls[0].PlanArches {
		if strings.HasPrefix(slot, "broken|") {
			t.Errorf("cooper plan included discover-failed package: %v", script.calls[0].PlanArches)
		}
	}
	if !strings.Contains(stdout.String(), "broken\t\tskip") {
		t.Errorf("stdout missing skip row for broken: %q", stdout.String())
	}
}

func TestRunReconcile_TrailerOK(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	want := "[hugo] result: ok (artifacts=1 built=1 skipped=0 failed=0 pruned=0)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout missing trailer %q:\n%s", want, stdout.String())
	}
}

func TestRunReconcile_TrailerAllSkipIsOK(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	be.hashes[p.Packages[0].Artifacts[0].Deb.BuildInputsHash] = true
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	if err := runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr)); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	want := "[hugo] result: ok (artifacts=1 built=0 skipped=1 failed=0 pruned=0)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout missing trailer %q:\n%s", want, stdout.String())
	}
}

func TestRunReconcile_TrailerMixedIsWarn(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t,
		stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64", "arm64"}},
	)
	script := &stubCooperBuildScript{
		failures: map[string]string{"hugo|arm64": "nfpm exploded"},
	}
	var stdout, stderr bytes.Buffer

	_ = runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	want := "[hugo] result: warn (artifacts=2 built=1 skipped=0 failed=1 pruned=0)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout missing trailer %q:\n%s", want, stdout.String())
	}
}

func TestRunReconcile_TrailerAllFailIsError(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{
		failures: map[string]string{"hugo|amd64": "nfpm exploded"},
	}
	var stdout, stderr bytes.Buffer

	_ = runReconcile(context.Background(), runOpts(be, p, script.Fn(), &stdout, &stderr))
	want := "[hugo] result: error (artifacts=1 built=0 skipped=0 failed=1 pruned=0)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout missing trailer %q:\n%s", want, stdout.String())
	}
}

func TestRunReconcile_TrailerNotPrintedInDryRun(t *testing.T) {
	be := newStubBackend()
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer

	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.DryRun = true
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "result:") {
		t.Errorf("trailer leaked into dry-run output:\n%s", stdout.String())
	}
}

// --- retain -----------------------------------------------------------------

// retainFixture: repo already holds hugo 0.138.0, 0.139.0 (cooper-built)
// plus a foreign 0.1.0 with no hash; the plan brings 0.140.0.
func retainFixture(t *testing.T) (*stubBackend, *plan.Plan) {
	t.Helper()
	be := newStubBackend()
	be.listings["hugo|amd64"] = []backend.Package{
		{Name: "hugo", Version: "0.138.0", Architecture: "amd64", BuildInputsHash: "sha256:h138", Ref: "k138"},
		{Name: "hugo", Version: "0.139.0", Architecture: "amd64", BuildInputsHash: "sha256:h139", Ref: "k139"},
		{Name: "hugo", Version: "0.1.0", Architecture: "amd64", Ref: "manual"},
	}
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	return be, p
}

func removedVersions(be *stubBackend) []string {
	out := []string{}
	for _, r := range be.removes {
		out = append(out, r.Version)
	}
	return out
}

func TestRunReconcile_RetainPrunesAfterImportBeforePublish(t *testing.T) {
	be, p := retainFixture(t)
	// Make the stub listing reflect the import so the prune sees it,
	// as a real backend would.
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 2

	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	// 0.140.0 isn't in the stub listing (static), so the two kept are
	// 0.139.0 and 0.138.0 — nothing pruned beyond retain=2; with the
	// listing simulating the import, 0.138.0 goes. Cover both shapes.
	if got := removedVersions(be); len(got) != 0 {
		t.Fatalf("static listing: removed %v, want none", got)
	}

	be2, p2 := retainFixture(t)
	be2.listings["hugo|amd64"] = append(be2.listings["hugo|amd64"],
		backend.Package{Name: "hugo", Version: "0.140.0", Architecture: "amd64", BuildInputsHash: p2.Packages[0].Artifacts[0].Deb.BuildInputsHash, Ref: "k140"})
	stdout.Reset()
	stderr.Reset()
	opts = runOpts(be2, p2, script.Fn(), &stdout, &stderr)
	opts.Retain = 2
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if got := removedVersions(be2); strings.Join(got, ",") != "0.138.0" {
		t.Fatalf("removed %v, want [0.138.0] (foreign 0.1.0 untouched)", got)
	}
	if be2.removes[0].Ref != "k138" {
		t.Errorf("Remove must receive the listed entry with its Ref: %+v", be2.removes[0])
	}
	if be2.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1", be2.publishCount)
	}
	out := stdout.String()
	if !strings.Contains(out, "pruned: hugo 0.138.0 amd64 (beyond_retain)") {
		t.Errorf("stdout missing prune line: %q", out)
	}
	if !strings.Contains(out, "[hugo] result: ok (artifacts=1 built=1 skipped=0 failed=0 pruned=1)") {
		t.Errorf("trailer: %q", out)
	}
	// Ordering: import happens before the prune.
	if len(be2.imports) != 1 {
		t.Fatalf("imports=%v", be2.imports)
	}
}

func TestRunReconcile_RetainSkippedWhenImportFails(t *testing.T) {
	be, p := retainFixture(t)
	be.listings["hugo|amd64"] = append(be.listings["hugo|amd64"],
		backend.Package{Name: "hugo", Version: "0.137.0", Architecture: "amd64", BuildInputsHash: "sha256:h137", Ref: "k137"})
	be.importErr["hugo_0.140.0_amd64.deb"] = errors.New("disk full")
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1

	if err := runReconcile(context.Background(), opts); err == nil {
		t.Fatal("expected error from failed import")
	}
	if got := removedVersions(be); len(got) != 0 {
		t.Errorf("pair did not settle; removed %v, want none", got)
	}
}

func TestRunReconcile_RetainSkippedWhenBuildFails(t *testing.T) {
	be, p := retainFixture(t)
	script := &stubCooperBuildScript{failures: map[string]string{"hugo|amd64": "nfpm exploded"}}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1

	_ = runReconcile(context.Background(), opts)
	if got := removedVersions(be); len(got) != 0 {
		t.Errorf("build failed; removed %v, want none", got)
	}
}

func TestRunReconcile_RetainPrunesOnHashInRepoSkip(t *testing.T) {
	// Re-run after a successful earlier reconcile: the plan's hash is
	// already present, the pair is settled, and retain still applies —
	// but the present entry itself is protected.
	be, p := retainFixture(t)
	hash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash
	be.hashes[hash] = true
	be.listings["hugo|amd64"] = append(be.listings["hugo|amd64"],
		backend.Package{Name: "hugo", Version: "0.140.0", Architecture: "amd64", BuildInputsHash: hash, Ref: "k140"})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1

	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if got := removedVersions(be); strings.Join(got, ",") != "0.138.0,0.139.0" {
		t.Fatalf("removed %v, want [0.138.0 0.139.0]", got)
	}
	// Prune-only run still publishes (aptly needs the regeneration).
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1 after prune-only run", be.publishCount)
	}
	if len(script.calls) != 0 {
		t.Errorf("cooper must not run on an all-skip plan")
	}
}

func TestRunReconcile_RetainSkippedOnVersionRegression(t *testing.T) {
	be := newStubBackend()
	be.listings["hugo|amd64"] = []backend.Package{
		{Name: "hugo", Version: "0.150.0", Architecture: "amd64", BuildInputsHash: "sha256:h150", Ref: "k150"},
		{Name: "hugo", Version: "0.149.0", Architecture: "amd64", BuildInputsHash: "sha256:h149", Ref: "k149"},
	}
	p := makePlan(t, stubPkg{name: "hugo", version: "0.140.0", arches: []string{"amd64"}})
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1

	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if got := removedVersions(be); len(got) != 0 {
		t.Errorf("regression pair must not be pruned; removed %v", got)
	}
}

func TestRunReconcile_RetainRemoveErrorIsWarnAndNonZero(t *testing.T) {
	be, p := retainFixture(t)
	hash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash
	be.listings["hugo|amd64"] = append(be.listings["hugo|amd64"],
		backend.Package{Name: "hugo", Version: "0.140.0", Architecture: "amd64", BuildInputsHash: hash, Ref: "k140"})
	be.removeErr["0.138.0"] = errors.New("aptly 500")
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	var logBuf bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1
	opts.AuditLog = audit.NewWriter(&logBuf)

	err := runReconcile(context.Background(), opts)
	if err == nil {
		t.Fatal("expected non-zero result from failed prune")
	}
	// Both removes attempted (oldest first); the first failed.
	if got := removedVersions(be); strings.Join(got, ",") != "0.138.0,0.139.0" {
		t.Fatalf("removes attempted %v", got)
	}
	// Publish still happened for the import.
	if be.publishCount != 1 {
		t.Errorf("publishCount=%d, want 1", be.publishCount)
	}
	if !strings.Contains(stderr.String(), "prune failed: hugo 0.138.0 amd64") {
		t.Errorf("stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "[hugo] result: warn (artifacts=1 built=1 skipped=0 failed=0 pruned=1)") {
		t.Errorf("trailer: %q", stdout.String())
	}
	// Audit: decision, import, prune(error), prune(ok), publish.
	var types, results []string
	for line := range strings.SplitSeq(strings.TrimRight(logBuf.String(), "\n"), "\n") {
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit JSON %q: %v", line, err)
		}
		types = append(types, e.Type)
		results = append(results, e.Result)
	}
	wantTypes := []string{audit.TypeDecision, audit.TypeImport, audit.TypePrune, audit.TypePrune, audit.TypePublish}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("audit types %v, want %v", types, wantTypes)
	}
	if results[2] != audit.ResultError || results[3] != audit.ResultOK {
		t.Errorf("prune results %v", results)
	}
}

func TestRunReconcile_RetainListErrorStillPublishes(t *testing.T) {
	// The retain re-list failing after a clean import must not strand
	// that import unpublished: same contract as a rejected removal.
	be, p := retainFixture(t)
	be.listErrAfterImport = errors.New("aptly 502")
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 1

	if err := runReconcile(context.Background(), opts); err == nil {
		t.Fatal("expected non-zero result from failed retain listing")
	}
	if len(be.imports) != 1 || be.publishCount != 1 {
		t.Fatalf("imports=%v publishCount=%d, want 1 import and 1 publish", be.imports, be.publishCount)
	}
	if got := removedVersions(be); len(got) != 0 {
		t.Errorf("removed %v, want none", got)
	}
	if !strings.Contains(stderr.String(), "prune failed: hugo amd64: list by name/arch: aptly 502") {
		t.Errorf("stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "[hugo] result: warn (artifacts=1 built=1 skipped=0 failed=0 pruned=0)") {
		t.Errorf("trailer: %q", stdout.String())
	}
}

// cleanupFixture is a settled re-run (hash in repo) where retain=1
// prunes 0.138.0 and 0.139.0.
func cleanupFixture(t *testing.T) (*stubBackend, *plan.Plan) {
	t.Helper()
	be, p := retainFixture(t)
	hash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash
	be.hashes[hash] = true
	be.listings["hugo|amd64"] = append(be.listings["hugo|amd64"],
		backend.Package{Name: "hugo", Version: "0.140.0", Architecture: "amd64", BuildInputsHash: hash, Ref: "k140"})
	return be, p
}

func TestRunReconcile_CleanupRunsAfterPublishWhenPruned(t *testing.T) {
	be, p := cleanupFixture(t)
	var stdout, stderr, logBuf bytes.Buffer
	opts := runOpts(be, p, (&stubCooperBuildScript{}).Fn(), &stdout, &stderr)
	opts.Retain = 1
	opts.Cleanup = true
	opts.AuditLog = audit.NewWriter(&logBuf)

	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if strings.Join(be.ops, ",") != "publish,cleanup" {
		t.Fatalf("ops=%v, want publish then cleanup", be.ops)
	}
	if !strings.Contains(stdout.String(), "db cleanup done") {
		t.Errorf("stdout: %q", stdout.String())
	}
	if !strings.Contains(logBuf.String(), `"type":"cleanup","backend":"stub","result":"ok"`) {
		t.Errorf("audit log missing cleanup event: %s", logBuf.String())
	}
}

func TestRunReconcile_CleanupSkippedWithoutPrunesOrFlag(t *testing.T) {
	// Flag set, but the run only imports: nothing was pruned.
	be, p := retainFixture(t)
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, (&stubCooperBuildScript{}).Fn(), &stdout, &stderr)
	opts.Cleanup = true
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if be.publishCount != 1 || be.cleanupCount != 0 {
		t.Errorf("import-only run: publish=%d cleanup=%d, want 1 and 0", be.publishCount, be.cleanupCount)
	}

	// Run prunes, but the flag is off.
	be, p = cleanupFixture(t)
	opts = runOpts(be, p, (&stubCooperBuildScript{}).Fn(), &stdout, &stderr)
	opts.Retain = 1
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatalf("runReconcile: %v", err)
	}
	if len(be.removes) != 2 || be.cleanupCount != 0 {
		t.Errorf("flag off: removes=%d cleanup=%d, want 2 and 0", len(be.removes), be.cleanupCount)
	}
}

func TestRunReconcile_CleanupErrorIsNonZeroAfterPublish(t *testing.T) {
	be, p := cleanupFixture(t)
	be.cleanupErr = errors.New("aptly 500")
	var stdout, stderr, logBuf bytes.Buffer
	opts := runOpts(be, p, (&stubCooperBuildScript{}).Fn(), &stdout, &stderr)
	opts.Retain = 1
	opts.Cleanup = true
	opts.AuditLog = audit.NewWriter(&logBuf)

	err := runReconcile(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "db cleanup") {
		t.Fatalf("want db cleanup error, got %v", err)
	}
	if !strings.Contains(stdout.String(), "publish updated") {
		t.Errorf("publish must have completed first: %q", stdout.String())
	}
	if !strings.Contains(logBuf.String(), `"type":"cleanup","backend":"stub","result":"error"`) {
		t.Errorf("audit log missing failed cleanup event: %s", logBuf.String())
	}
}

func TestRunReconcile_RetainDisabledNeverRemoves(t *testing.T) {
	be, p := retainFixture(t)
	hash := p.Packages[0].Artifacts[0].Deb.BuildInputsHash
	be.hashes[hash] = true
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	// Retain left at 0.
	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(be.removes) != 0 {
		t.Errorf("retain=0 removed %v", removedVersions(be))
	}
}

func TestRunReconcile_DryRunPredictsPrunes(t *testing.T) {
	be, p := retainFixture(t)
	script := &stubCooperBuildScript{}
	var stdout, stderr bytes.Buffer
	opts := runOpts(be, p, script.Fn(), &stdout, &stderr)
	opts.Retain = 2
	opts.DryRun = true

	if err := runReconcile(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(be.removes) != 0 || len(be.imports) != 0 || be.publishCount != 0 {
		t.Fatalf("dry-run mutated: removes=%v imports=%v publish=%d", be.removes, be.imports, be.publishCount)
	}
	// Predicted: once 0.140.0 lands, retain=2 keeps 0.140.0 + 0.139.0.
	if !strings.Contains(stdout.String(), "hugo\tamd64\tprune\t0.138.0: upstream 0.138.0 ranks #3 newest; retain=2") {
		t.Errorf("dry-run prediction missing: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "0.139.0:") {
		t.Errorf("0.139.0 must be kept: %q", stdout.String())
	}
}

func TestPredictPrunes_IgnoresUnsettledDecisions(t *testing.T) {
	be, _ := retainFixture(t)
	decisions := []policy.Decision{
		{PackageName: "hugo", Arch: "amd64", Action: policy.ActionSkip, Code: policy.CodeVersionRegression, PlanVersion: "0.1.0"},
		{PackageName: "broken", Action: policy.ActionSkip, Code: policy.CodeDiscoverFailed},
	}
	got, err := predictPrunes(context.Background(), be, decisions, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("predicted %+v for unsettled decisions", got)
	}
}

func TestValidateRetain(t *testing.T) {
	var buf bytes.Buffer
	if err := validateRetain(-1, "aptly", &buf); err == nil {
		t.Error("negative retain accepted")
	}
	if err := validateRetain(3, "aptly", &buf); err != nil || buf.Len() != 0 {
		t.Errorf("aptly: err=%v note=%q", err, buf.String())
	}
	if err := validateRetain(3, "reprepro-ssh", &buf); err != nil || !strings.Contains(buf.String(), "reprepro") {
		t.Errorf("reprepro: err=%v note=%q", err, buf.String())
	}
	buf.Reset()
	if err := validateRetain(0, "reprepro-local", &buf); err != nil || buf.Len() != 0 {
		t.Errorf("retain=0 on reprepro should be silent: %q", buf.String())
	}
}
