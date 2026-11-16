package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/drayman/audit"
)

// cmdPublish implements `drayman publish` — regenerate the target
// repo's published metadata without importing anything.
//
// reconcile and import only publish when the run itself changed the
// repo, so a run whose imports landed but whose publish then failed
// (signing hiccup, proxy timeout) has no way to retry: the next run
// sees every artifact present and does nothing. This is that retry.
// With --aptly-db-cleanup it also re-runs a cleanup that failed.
func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman publish --backend <kind> [backend flags] [--audit-log PATH]")
		fs.PrintDefaults()
	}
	bf := registerBackendFlags(fs)
	auditPath := fs.String("audit-log", "", "append JSONL audit events to this path (publish, cleanup)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	be, err := bf.resolveBackend()
	if err != nil {
		return err
	}
	if bf.publishRequiresDistribution() && *bf.publishDist == "" {
		return errors.New("--publish-distribution is required for --backend aptly")
	}

	var auditLog *audit.Logger
	if *auditPath != "" {
		auditLog, err = audit.Open(*auditPath)
		if err != nil {
			return err
		}
		defer auditLog.Close()
	}
	return publishAndCleanup(context.Background(), be, *bf.kind, auditLog, os.Stdout, *bf.aptlyDBCleanup)
}
