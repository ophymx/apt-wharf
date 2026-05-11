package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ophymx/apt-signpost/internal/drayman/aptly"
	"github.com/ophymx/apt-signpost/internal/drayman/policy"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// cmdReconcile is drayman's end-to-end flow: read a cooper discover
// plan, query aptly for already-imported artifacts, group surviving
// artifacts by chosen debian-revision, exec cooper build per group,
// upload each resulting .deb into aptly, and trigger a publish
// regeneration. Per-artifact failures are tolerated; the process exits
// non-zero only when any artifact failed.
func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman reconcile <PLAN_FILE> --aptly-url URL --repo NAME [flags]")
		fs.PrintDefaults()
	}
	aptlyURL := fs.String("aptly-url", "", "base URL of aptly API (required)")
	repo := fs.String("repo", "", "aptly local repository name (required)")
	cooperBin := fs.String("cooper-bin", "cooper", "path to cooper binary")
	outDir := fs.String("out-dir", "", "where cooper builds .debs (default: ephemeral; cleaned on exit)")
	publishPrefix := fs.String("publish-prefix", ".", "aptly publish prefix")
	publishDist := fs.String("publish-distribution", "", "aptly publish distribution (required unless --dry-run)")
	skipSigning := fs.Bool("skip-signing", false, "set Signing.Skip on the publish update (for aptly servers without gpg keys)")
	dryRun := fs.Bool("dry-run", false, "query and decide but don't build, upload, or publish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *aptlyURL == "" || *repo == "" {
		fs.Usage()
		return errors.New("--aptly-url and --repo are required")
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional PLAN_FILE argument (use - for stdin)")
	}
	if !*dryRun && *publishDist == "" {
		return errors.New("--publish-distribution is required (omit only with --dry-run)")
	}

	p, err := readPlanInput(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}

	ctx := context.Background()
	client := aptly.New(*aptlyURL, &http.Client{Timeout: 5 * time.Minute})

	decisions, err := policy.Decide(ctx, client, *repo, p)
	if err != nil {
		return err
	}
	printDecisions(decisions)

	if *dryRun {
		fmt.Println("dry-run: no build, upload, or publish.")
		return nil
	}

	// Set up an out-dir for cooper's .deb output. If the user didn't
	// pick one, we use an ephemeral temp dir and clean up on exit.
	cooperOutDir := *outDir
	cleanupOut := false
	if cooperOutDir == "" {
		tmp, err := os.MkdirTemp("", "drayman-out-")
		if err != nil {
			return fmt.Errorf("mkdir out-dir: %w", err)
		}
		cooperOutDir = tmp
		cleanupOut = true
		defer func() {
			if cleanupOut {
				_ = os.RemoveAll(cooperOutDir)
			}
		}()
	}

	// Cooper's --work-dir must be absolute or nfpm's relative
	// ${ASSETS} expansion doubles paths. Always allocate a fresh
	// staging root and clean up on exit.
	cooperWorkDir, err := os.MkdirTemp("", "drayman-work-")
	if err != nil {
		return fmt.Errorf("mkdir work-dir: %w", err)
	}
	defer os.RemoveAll(cooperWorkDir)

	// Group decisions by revision so one cooper build invocation can
	// handle every artifact at the same N. The 0-bucket is bare; >0
	// gets --revision N.
	groups := groupByRevision(decisions)
	var anyFailed bool
	var anyUploaded bool
	uploadedDir := "drayman-" + newRunID()

	for _, rev := range sortedRevisions(groups) {
		group := groups[rev]
		filtered := filterPlanForDecisions(p, group)
		annotated, err := execCooperBuild(ctx, *cooperBin, filtered, cooperOutDir, cooperWorkDir, rev)
		if err != nil {
			return fmt.Errorf("cooper build (revision %d): %w", rev, err)
		}
		built, failed := collectArtifacts(annotated)
		anyFailed = anyFailed || failed
		for _, a := range built {
			if err := uploadArtifact(ctx, client, uploadedDir, a); err != nil {
				return fmt.Errorf("upload %s: %w", a.path, err)
			}
			anyUploaded = true
		}
	}

	if anyUploaded {
		// One import call drains the staged directory into the repo.
		// forceReplace=true so a re-run that produces the same
		// filename overwrites cleanly rather than erroring; aptly's
		// design supports same-package re-import.
		rep, err := client.ImportFromDir(ctx, *repo, uploadedDir, true)
		if err != nil {
			return fmt.Errorf("import staged: %w", err)
		}
		for _, msg := range rep.Report.Added {
			fmt.Printf("imported: %s\n", msg)
		}
		for _, f := range rep.FailedFiles {
			fmt.Fprintf(os.Stderr, "import failed: %s\n", f)
			anyFailed = true
		}

		if err := client.PublishUpdate(ctx, *publishPrefix, *publishDist, aptly.PublishUpdateOpts{SkipSigning: *skipSigning}); err != nil {
			return fmt.Errorf("publish update: %w", err)
		}
		fmt.Println("publish updated")
	} else {
		fmt.Println("nothing to import (all artifacts already in repo)")
	}

	if anyFailed {
		return errors.New("one or more artifacts failed to build or import")
	}
	return nil
}

// printDecisions writes the same one-line-per-decision format peek
// uses, so reconcile's "what I'm about to do" preamble looks the same
// as `drayman peek`.
func printDecisions(decisions []policy.Decision) {
	for _, d := range decisions {
		fmt.Printf("%s\t%s\t%s\t%s\n", d.PackageName, d.Arch, d.Action, d.Reason)
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
	// tiny inline sort (avoids importing "sort" just for ints).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
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

// builtArtifact captures the minimal info needed to upload a .deb to
// aptly: the local path cooper wrote and the basename to stage under.
type builtArtifact struct {
	path     string
	basename string
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
	return &annotated, nil
}

// collectArtifacts pulls every successful artifact's path out of an
// annotated cooper-build plan. The second return value is true when
// any artifact failed (the caller propagates that to the exit code).
func collectArtifacts(annotated *plan.Plan) ([]builtArtifact, bool) {
	var built []builtArtifact
	var failed bool
	for _, pkg := range annotated.Packages {
		for _, a := range pkg.Artifacts {
			if a.Result == plan.ResultError {
				fmt.Fprintf(os.Stderr, "build failed: %s %s: %s\n", pkg.Name, a.Arch, a.Error.Message)
				failed = true
				continue
			}
			if a.Deb.Path == nil {
				continue
			}
			built = append(built, builtArtifact{
				path:     *a.Deb.Path,
				basename: filepath.Base(*a.Deb.Path),
			})
		}
	}
	return built, failed
}

// uploadArtifact stages a single .deb under uploadedDir on the aptly
// server. The actual import-into-repo happens once after every upload,
// not per artifact.
func uploadArtifact(ctx context.Context, c *aptly.Client, uploadedDir string, a builtArtifact) error {
	_, err := c.UploadFile(ctx, uploadedDir, a.path)
	if err != nil {
		return err
	}
	fmt.Printf("uploaded: %s\n", a.basename)
	return nil
}

// newRunID returns 8 hex chars, suitable for namespacing aptly upload
// directories per drayman invocation. Collision-resistant enough that
// concurrent drayman runs don't tread on each other's staging.
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

