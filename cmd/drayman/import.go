package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pault.ag/go/debian/deb"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/drayman/audit"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/internal/drayman/policy"
)

// importOpts is the dependency set runImport needs. cmdImport builds
// it from CLI flags; tests construct it directly with stubs.
type importOpts struct {
	Backend     backend.Backend
	BackendKind string // for audit-log entries
	Debs        []policy.Deb
	DryRun      bool
	Retain      int           // >0 enables the retain policy for the pairs named by Debs
	Cleanup     bool          // reclaim backend storage after a run that pruned (--aptly-db-cleanup)
	AuditLog    *audit.Logger // optional; nil disables audit logging
	Stdout      io.Writer
	Stderr      io.Writer
}

// cmdImport implements `drayman import <DEB>...` — haul .debs that
// something other than cooper built into the target repo. There is no
// plan and no build step: each file's control fields say what it is,
// any (Package, Version, Architecture) the repo already lists is
// skipped, and the rest are handed to the backend in one batch and
// published once. See internal/drayman/policy/import.go for why this
// path keys on version rather than X-Cooper-Build-Inputs-Hash.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman import <DEB>... --backend <kind> [backend flags] [--retain N] [--dry-run] [--audit-log PATH]")
		fs.PrintDefaults()
	}
	bf := registerBackendFlags(fs)
	dryRun := fs.Bool("dry-run", false, "query and decide but don't upload, prune, or publish")
	auditPath := fs.String("audit-log", "", "append JSONL audit events to this path (decisions, imports, prunes, publish, cleanup)")
	retain := registerRetainFlag(fs)
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if err := validateRetain(*retain, *bf.kind, os.Stderr); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("expected at least one .deb path")
	}
	be, err := bf.resolveBackend()
	if err != nil {
		return err
	}
	if !*dryRun && bf.publishRequiresDistribution() && *bf.publishDist == "" {
		return errors.New("--publish-distribution is required for --backend aptly (omit only with --dry-run)")
	}

	// Read every control file before touching the backend: a bad
	// path or a non-.deb should fail the run before a multi-GB
	// sibling starts uploading.
	debs := make([]policy.Deb, 0, fs.NArg())
	for _, path := range fs.Args() {
		d, err := readDeb(path)
		if err != nil {
			return err
		}
		debs = append(debs, d)
	}

	var auditLog *audit.Logger
	if *auditPath != "" {
		auditLog, err = audit.Open(*auditPath)
		if err != nil {
			return err
		}
		defer auditLog.Close()
	}

	return runImport(context.Background(), importOpts{
		Backend:     be,
		BackendKind: *bf.kind,
		Debs:        debs,
		DryRun:      *dryRun,
		Retain:      *retain,
		Cleanup:     *bf.aptlyDBCleanup,
		AuditLog:    auditLog,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	})
}

// readDeb identifies a .deb by its control file. Only the ar headers
// and the control member are read, never the data member, so this is
// cheap however large the package is.
func readDeb(path string) (policy.Deb, error) {
	d, closer, err := deb.LoadFile(path)
	if err != nil {
		return policy.Deb{}, fmt.Errorf("%s: read control: %w", path, err)
	}
	defer closer()
	// Raw field text, not the library's parsed forms: the version has
	// to match what the repo will list, character for character.
	v := d.Control.Values
	out := policy.Deb{Path: path, Name: v["Package"], Version: v["Version"], Arch: v["Architecture"]}
	if out.Name == "" || out.Version == "" || out.Arch == "" {
		return policy.Deb{}, fmt.Errorf("%s: control file lacks Package, Version, or Architecture", path)
	}
	return out, nil
}

// pair is one (Package, Architecture) named by the .debs of a run.
type pair struct{ name, arch string }

// runImport is the import flow, free of CLI concerns; tests drive it
// directly with a stub backend.
func runImport(ctx context.Context, opts importOpts) error {
	if err := checkDistinct(opts.Debs); err != nil {
		return err
	}
	decisions, err := policy.DecideImports(ctx, opts.Backend, opts.Debs)
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

	// Every pair the run names, in first-seen order, with the
	// versions it names for that pair. Those versions are what retain
	// protects in place of a hash.
	var pairs []pair
	versions := map[pair][]string{}
	var importing []policy.Deb
	for i, d := range decisions {
		pr := pair{d.PackageName, d.Arch}
		if _, ok := versions[pr]; !ok {
			pairs = append(pairs, pr)
		}
		versions[pr] = append(versions[pr], d.PlanVersion)
		if d.Action == policy.ActionImport {
			importing = append(importing, opts.Debs[i])
		}
	}

	if opts.DryRun {
		// Predict what retain would remove once the imports land:
		// the repo's listing plus a synthetic entry per import.
		for _, pr := range pairs {
			var extra []backend.Package
			for _, deb := range importing {
				if deb.Name == pr.name && deb.Arch == pr.arch {
					extra = append(extra, backend.Package{Name: deb.Name, Version: deb.Version, Architecture: deb.Arch})
				}
			}
			prunes, err := policy.PrunesForAll(ctx, opts.Backend, pr.name, pr.arch, opts.Retain, versions[pr], extra...)
			if err != nil {
				return fmt.Errorf("retain: %w", err)
			}
			printPrunesTo(opts.Stdout, prunes)
		}
		fmt.Fprintln(opts.Stdout, "dry-run: no upload, prune, or publish.")
		return nil
	}

	var anyFailed, anyImported bool
	unsettled := map[pair]bool{}

	if len(importing) > 0 {
		paths := make([]string, len(importing))
		for i, deb := range importing {
			paths[i] = deb.Path
		}
		// One batch: files that belong together reach the backend
		// in a single operation, and aptly publishes none of them
		// unless all were accepted.
		importErr := opts.Backend.Import(ctx, paths...)
		if importErr != nil {
			if opts.AuditLog != nil {
				for _, path := range paths {
					_ = opts.AuditLog.Import(path, filepath.Base(path), importErr)
				}
			}
			return fmt.Errorf("import: %w (nothing was pruned and no publish was requested; re-run to retry — versions that did land are skipped)", importErr)
		}
		// Accepted is not the same as listed: reprepro exits 0 when
		// it declines a version it considers superseded, or files it
		// under another component. Re-list before calling it done.
		for _, deb := range importing {
			base := filepath.Base(deb.Path)
			var verifyErr error
			listed, err := opts.Backend.ListByNameArch(ctx, deb.Name, deb.Arch)
			switch {
			case err != nil:
				verifyErr = fmt.Errorf("could not verify: %w", err)
			case !policy.HasVersion(listed, deb.Version):
				verifyErr = fmt.Errorf("the backend accepted it but the repo does not list %s %s %s", deb.Name, deb.Version, deb.Arch)
			}
			if opts.AuditLog != nil {
				_ = opts.AuditLog.Import(deb.Path, base, verifyErr)
			}
			if verifyErr != nil {
				fmt.Fprintf(opts.Stderr, "import failed: %s: %v\n", base, verifyErr)
				unsettled[pair{deb.Name, deb.Arch}] = true
				anyFailed = true
				continue
			}
			fmt.Fprintf(opts.Stdout, "imported: %s\n", base)
			anyImported = true
		}
	}

	// Retain: same placement as reconcile (after imports, before the
	// single publish), but over every entry of the pair, since
	// prebuilt packages carry no hash to scope by. Only pairs whose
	// versions are all verifiably in the repo are touched.
	var anyPruned bool
	for _, pr := range pairs {
		if unsettled[pr] {
			continue
		}
		prunes, err := policy.PrunesForAll(ctx, opts.Backend, pr.name, pr.arch, opts.Retain, versions[pr])
		if err != nil {
			fmt.Fprintf(opts.Stderr, "prune failed: %v\n", err)
			anyFailed = true
			continue
		}
		pruned, failed := removePrunes(ctx, opts.Backend, opts.AuditLog, opts.Stdout, opts.Stderr, prunes)
		anyPruned = anyPruned || pruned > 0
		anyFailed = anyFailed || failed > 0
	}

	if anyImported || anyPruned {
		if err := publishAndCleanup(ctx, opts.Backend, opts.BackendKind, opts.AuditLog, opts.Stdout, opts.Cleanup && anyPruned); err != nil {
			return err
		}
	} else if !anyFailed {
		fmt.Fprintln(opts.Stdout, "nothing to import (every version already in repo)")
	}

	if anyFailed {
		return errors.New("one or more packages failed to import or prune")
	}
	return nil
}

// checkDistinct rejects inputs the batch can't represent: the same
// (Package, Version, Architecture) twice (which bytes win?), or two
// files sharing a basename (they would overwrite each other in the
// backend's staging area).
func checkDistinct(debs []policy.Deb) error {
	type triple struct{ name, version, arch string }
	byTriple := map[triple]string{}
	byBase := map[string]string{}
	for _, d := range debs {
		t := triple{d.Name, d.Version, d.Arch}
		if prev, ok := byTriple[t]; ok {
			return fmt.Errorf("%s and %s are both %s %s %s; pass one", prev, d.Path, d.Name, d.Version, d.Arch)
		}
		byTriple[t] = d.Path
		base := filepath.Base(d.Path)
		if prev, ok := byBase[base]; ok {
			return fmt.Errorf("%s and %s share the file name %s; rename one", prev, d.Path, base)
		}
		byBase[base] = d.Path
	}
	return nil
}
