package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ophymx/apt-signpost/internal/drayman/audit"
	"github.com/ophymx/apt-signpost/internal/drayman/backend"
	"github.com/ophymx/apt-signpost/pkg/plan"
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

	hashErr    error
	listErr    error
	importErr  map[string]error // keyed by basename; missing key → nil
	publishErr error

	imports      []string // deb paths in call order
	publishCount int
}

func newStubBackend() *stubBackend {
	return &stubBackend{
		hashes:    map[string]bool{},
		listings:  map[string][]backend.Package{},
		importErr: map[string]error{},
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
	return s.listings[name+"|"+arch], nil
}

func (s *stubBackend) Import(_ context.Context, debPath string) error {
	s.imports = append(s.imports, debPath)
	if err, ok := s.importErr[filepath.Base(debPath)]; ok {
		return err
	}
	return nil
}

func (s *stubBackend) Publish(_ context.Context) error {
	s.publishCount++
	return s.publishErr
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
