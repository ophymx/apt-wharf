package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ophymx/apt-wharf/internal/drayman/policy"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// cmdPeek implements `drayman peek <PLAN>` — a read-only dry-run that
// queries the target repo and prints the decision drayman would make
// per artifact, without touching cooper or invoking any mutation.
// Backend selected by --backend (aptly | reprepro-local | reprepro-ssh).
func cmdPeek(args []string) error {
	fs := flag.NewFlagSet("peek", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman peek <PLAN_FILE> --backend <kind> [backend flags]")
		fs.PrintDefaults()
	}
	bf := registerBackendFlags(fs)
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
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

	p, err := readPlanInput(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}

	decisions, err := policy.Decide(context.Background(), be, p)
	if err != nil {
		return err
	}
	for _, d := range decisions {
		fmt.Printf("%s\t%s\t%s\t%s\n", d.PackageName, d.Arch, d.Action, d.Reason)
	}
	return nil
}

// readPlanInput parses a cooper discover JSON plan from a path or
// stdin (`-`). Mirrors cmd/cooper/build.go's reader so both tools
// accept the same input shapes.
func readPlanInput(path string) (*plan.Plan, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var p plan.Plan
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return &p, nil
}
