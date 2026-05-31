package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/drayman/audit"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/internal/drayman/policy"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// cooperBuildFn is the contract drayman's reconcile loop has with
// "the thing that turns a plan into .debs." In production it shells
// out to `cooper build`; tests substitute a stub that returns canned
// annotated plans without touching the filesystem.
type cooperBuildFn func(ctx context.Context, p *plan.Plan, outDir, workDir string, revision int) (*plan.Plan, error)

// reconcileOpts is the dependency set runReconcile needs. cmdReconcile
// constructs it from CLI flags; tests construct it directly with
// stubs.
type reconcileOpts struct {
	Backend     backend.Backend
	BackendKind string // for audit-log entries
	Plan        *plan.Plan
	CooperBuild cooperBuildFn
	OutDir      string // pre-resolved absolute path
	WorkDir     string // pre-resolved absolute path
	DryRun      bool
	AuditLog    *audit.Logger // optional; nil disables audit logging
	Stdout      io.Writer
	Stderr      io.Writer
}

// cmdReconcile is drayman's end-to-end flow: read a cooper discover
// plan, query the target repo for already-imported artifacts, group
// surviving artifacts by chosen debian-revision, exec cooper build
// per group, import each resulting .deb into the backend, and
// trigger a publish regeneration. Per-artifact failures are
// tolerated; the process exits non-zero only when any artifact
// failed.
func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman reconcile <PLAN_FILE> --backend <kind> [backend flags] [common flags]")
		fs.PrintDefaults()
	}
	bf := registerBackendFlags(fs)
	cooperBin := fs.String("cooper-bin", "cooper", "path to cooper binary")
	outDir := fs.String("out-dir", "", "where cooper builds .debs (default: ephemeral; cleaned on exit)")
	dryRun := fs.Bool("dry-run", false, "query and decide but don't build, import, or publish")
	auditPath := fs.String("audit-log", "", "append JSONL audit events to this path (decisions, imports, publish)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional PLAN_FILE argument (use - for stdin)")
	}
	be, err := bf.resolveBackend()
	if err != nil {
		return err
	}
	if !*dryRun && bf.publishRequiresDistribution() && *bf.publishDist == "" {
		return errors.New("--publish-distribution is required for --backend aptly (omit only with --dry-run)")
	}

	p, err := readPlanInput(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}

	var auditLog *audit.Logger
	if *auditPath != "" {
		auditLog, err = audit.Open(*auditPath)
		if err != nil {
			return err
		}
		defer auditLog.Close()
	}

	// Resolve cooper-build's out-dir + work-dir to absolute paths
	// before reconcile runs. nfpm runs with cwd=staging-dir, so a
	// relative path would be re-rooted at the staging dir and the
	// asset would land in the wrong place.
	cooperOutDir := *outDir
	if cooperOutDir == "" {
		tmp, err := os.MkdirTemp("", "drayman-out-")
		if err != nil {
			return fmt.Errorf("mkdir out-dir: %w", err)
		}
		cooperOutDir = tmp
		defer os.RemoveAll(tmp)
	} else {
		abs, err := filepath.Abs(cooperOutDir)
		if err != nil {
			return fmt.Errorf("resolve out-dir: %w", err)
		}
		cooperOutDir = abs
	}
	cooperWorkDir, err := os.MkdirTemp("", "drayman-work-")
	if err != nil {
		return fmt.Errorf("mkdir work-dir: %w", err)
	}
	defer os.RemoveAll(cooperWorkDir)

	// Production cooperBuild execs the real cooper binary; tests
	// inject a stub.
	exec := func(ctx context.Context, plan *plan.Plan, outDir, workDir string, revision int) (*plan.Plan, error) {
		return execCooperBuild(ctx, *cooperBin, plan, outDir, workDir, revision)
	}

	return runReconcile(context.Background(), reconcileOpts{
		Backend:     be,
		BackendKind: *bf.kind,
		Plan:        p,
		CooperBuild: exec,
		OutDir:      cooperOutDir,
		WorkDir:     cooperWorkDir,
		DryRun:      *dryRun,
		AuditLog:    auditLog,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	})
}

// runReconcile is the orchestration loop, free of CLI concerns and
// filesystem ownership. Tests drive this directly with stubbed
// backends and cooperBuild functions; cmdReconcile assembles a real
// opts from flags and delegates here.
func runReconcile(ctx context.Context, opts reconcileOpts) error {
	decisions, err := policy.Decide(ctx, opts.Backend, opts.Plan)
	if err != nil {
		return err
	}
	printDecisionsTo(opts.Stdout, decisions)
	if opts.AuditLog != nil {
		for _, d := range decisions {
			if err := opts.AuditLog.Decision(d); err != nil {
				return fmt.Errorf("audit log decision: %w", err)
			}
		}
	}

	if opts.DryRun {
		fmt.Fprintln(opts.Stdout, "dry-run: no build, upload, or publish.")
		return nil
	}

	// Group decisions by revision so one cooper build invocation can
	// handle every artifact at the same N. The 0-bucket is bare; >0
	// gets --revision N.
	groups := groupByRevision(decisions)
	var anyFailed bool
	var anyImported bool

	for _, rev := range sortedRevisions(groups) {
		group := groups[rev]
		filtered := filterPlanForDecisions(opts.Plan, group)
		annotated, err := opts.CooperBuild(ctx, filtered, opts.OutDir, opts.WorkDir, rev)
		if err != nil {
			return fmt.Errorf("cooper build (revision %d): %w", rev, err)
		}
		built, failed := collectArtifactsTo(opts.Stderr, annotated)
		anyFailed = anyFailed || failed
		for _, debPath := range built {
			name := filepath.Base(debPath)
			importErr := opts.Backend.Import(ctx, debPath)
			if opts.AuditLog != nil {
				_ = opts.AuditLog.Import(debPath, name, importErr)
			}
			if importErr != nil {
				fmt.Fprintf(opts.Stderr, "import failed: %s: %v\n", name, importErr)
				anyFailed = true
				continue
			}
			fmt.Fprintf(opts.Stdout, "imported: %s\n", name)
			anyImported = true
		}
	}

	if anyImported {
		publishErr := opts.Backend.Publish(ctx)
		if opts.AuditLog != nil {
			_ = opts.AuditLog.Publish(opts.BackendKind, publishErr)
		}
		if publishErr != nil {
			return fmt.Errorf("publish: %w", publishErr)
		}
		fmt.Fprintln(opts.Stdout, "publish updated")
	} else {
		fmt.Fprintln(opts.Stdout, "nothing to import (all artifacts already in repo)")
	}

	if anyFailed {
		return errors.New("one or more artifacts failed to build or import")
	}
	return nil
}

// printDecisionsTo writes the same one-line-per-decision format peek
// uses, so reconcile's "what I'm about to do" preamble looks the same
// as `drayman peek`. Takes an io.Writer so tests can capture output.
func printDecisionsTo(w io.Writer, decisions []policy.Decision) {
	for _, d := range decisions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.PackageName, d.Arch, d.Action, d.Reason)
	}
}

// groupByRevision buckets build decisions by their target revision
// number. Skip decisions are dropped (no work to do).
func groupByRevision(decisions []policy.Decision) map[int][]policy.Decision {
	out := map[int][]policy.Decision{}
	for _, d := range decisions {
		if d.Action != policy.ActionBuild {
			continue
		}
		out[d.Revision] = append(out[d.Revision], d)
	}
	return out
}

// sortedRevisions returns the keys of groups in ascending order so
// output is deterministic regardless of map iteration order.
func sortedRevisions(groups map[int][]policy.Decision) []int {
	out := make([]int, 0, len(groups))
	for k := range groups {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// filterPlanForDecisions returns a deep-ish copy of p containing only
// the (package, artifact) pairs named in decisions. Packages whose
// artifacts are all filtered out are dropped entirely.
func filterPlanForDecisions(p *plan.Plan, decisions []policy.Decision) *plan.Plan {
	keep := map[string]bool{} // "<package>|<arch>" → true
	for _, d := range decisions {
		keep[d.PackageName+"|"+d.Arch] = true
	}
	out := *p
	out.Packages = nil
	for _, pkg := range p.Packages {
		if pkg.Result != plan.ResultOK {
			continue
		}
		var arts []plan.Artifact
		for _, a := range pkg.Artifacts {
			if keep[pkg.Name+"|"+a.Arch] {
				arts = append(arts, a)
			}
		}
		if len(arts) == 0 {
			continue
		}
		newPkg := pkg
		newPkg.Artifacts = arts
		out.Packages = append(out.Packages, newPkg)
	}
	return &out
}

// execCooperBuild runs cooper as a subprocess, feeding p over stdin
// and reading the annotated plan from stdout. revision > 0 adds
// --revision N to the argv. workDir is the staging root cooper uses
// (must be an absolute path — cooper's default ./.cooper-work is
// relative, which breaks nfpm's ${ASSETS} expansion when nfpm runs
// with cwd = staging dir).
//
// Cooper exits non-zero when any artifact failed but still writes a
// valid annotated plan to stdout — so the exit code is informational,
// not fatal. We bubble up only when stdout fails to parse as JSON
// (cooper crashed before emitting output, or hit a usage error).
// A non-zero exit alongside parseable output is logged to stderr;
// callers still trust the per-artifact Result fields.
func execCooperBuild(ctx context.Context, cooperBin string, p *plan.Plan, outDir, workDir string, revision int) (*plan.Plan, error) {
	args := []string{"build", "--out-dir", outDir, "--work-dir", workDir}
	if revision > 0 {
		args = append(args, "--revision", fmt.Sprint(revision))
	}
	args = append(args, "-") // stdin
	cmd := exec.CommandContext(ctx, cooperBin, args...)
	stdin, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal plan: %w", err)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = os.Stderr

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	runErr := cmd.Run()

	var annotated plan.Plan
	if err := json.Unmarshal(stdout.Bytes(), &annotated); err != nil {
		// Couldn't parse output — cooper failed before emitting JSON,
		// or emitted something else entirely. The exit code (in
		// runErr) carries the relevant context.
		if runErr != nil {
			return nil, fmt.Errorf("exec %s: %w (no valid plan on stdout)", cooperBin, runErr)
		}
		return nil, fmt.Errorf("decode cooper output: %w", err)
	}
	if runErr != nil {
		// Output parsed cleanly, so per-artifact Result fields will
		// surface real failures. Note the exit code so the operator
		// knows it wasn't a clean run.
		fmt.Fprintf(os.Stderr, "cooper exited non-zero (revision %d): %v\n", revision, runErr)
	}
	return &annotated, nil
}

// collectArtifactsTo pulls every successful artifact's path out of an
// annotated cooper-build plan. The second return value is true when
// any artifact failed (the caller propagates that to the exit code).
// stderr is injected so tests can capture build-failed messages.
func collectArtifactsTo(stderr io.Writer, annotated *plan.Plan) ([]string, bool) {
	var built []string
	var failed bool
	for _, pkg := range annotated.Packages {
		for _, a := range pkg.Artifacts {
			if a.Result == plan.ResultError {
				fmt.Fprintf(stderr, "build failed: %s %s: %s\n", pkg.Name, a.Arch, a.Error.Message)
				failed = true
				continue
			}
			if a.Deb.Path == nil {
				continue
			}
			built = append(built, *a.Deb.Path)
		}
	}
	return built, failed
}
