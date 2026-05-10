package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ophymx/apt-signpost/internal/cooper/build"
	"github.com/ophymx/apt-signpost/pkg/plan"
)

// cmdBuild implements `cooper build <JSON_FILE>` per cooper-design.md
// §"CLI". Reads a discover-shaped JSON plan, resolves each artifact into
// a .deb (download → extract → stage → exec nfpm), and re-emits the
// plan annotated with deb.path / deb.sha256 on every successful build.
//
// Process exit code is non-zero if any artifact failed.
func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: cooper build <JSON_FILE> [--out-dir DIR] [--work-dir DIR] [--stage-only | --keep-work]")
	}
	outDir := fs.String("out-dir", "./dist", "where .debs land")
	workDir := fs.String("work-dir", "./.cooper-work", "staging root")
	stageOnly := fs.Bool("stage-only", false, "stage artifacts but skip nfpm exec; keep work dir")
	keepWork := fs.Bool("keep-work", false, "build normally but skip work-dir cleanup")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional JSON_FILE argument (use - for stdin)")
	}
	if *stageOnly && *keepWork {
		return errors.New("--stage-only and --keep-work are mutually exclusive")
	}

	in, err := readPlanInput(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}

	res, err := build.Run(context.Background(), in, build.Options{
		OutDir:    *outDir,
		WorkDir:   *workDir,
		StageOnly: *stageOnly,
		KeepWork:  *keepWork,
	})
	if err != nil {
		return err
	}

	out, err := build.EmitJSON(res)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return err
	}

	if build.AnyArtifactFailed(res) {
		return errors.New("one or more artifacts failed to build")
	}
	return nil
}

// readPlanInput parses a JSON plan from a path or stdin (`-`).
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
