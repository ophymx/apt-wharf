package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ophymx/apt-wharf/internal/chandler/config"
	"github.com/ophymx/apt-wharf/internal/chandler/discover"
	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

const chandlerVersion = "0.1.0"

// cmdDiscover implements `chandler discover <CONFIG>`. Reads the
// chandler YAML, fetches keys, renders sources, and emits a plan.Plan
// JSON document on stdout (or -o OUTPUT).
//
// Output is compatible with `cooper build -`; pipe the two together
// to build .debs end-to-end.
func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: chandler discover <CONFIG> [-o OUTPUT]")
	}
	out := fs.String("o", "-", "output path (`-` for stdout)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional CONFIG argument")
	}

	cfg, err := config.Load(fs.Arg(0))
	if err != nil {
		return err
	}

	p, err := discover.Run(context.Background(), cfg, discover.Options{
		Tool: plan.Tool{
			Name:           "chandler",
			Version:        chandlerVersion,
			FormatRevision: plan.FormatRevision,
		},
	})
	if err != nil {
		return err
	}

	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	body = append(body, '\n')
	return writeOut(*out, body)
}

func writeOut(path string, body []byte) error {
	var w io.Writer = os.Stdout
	if path != "-" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	_, err := w.Write(body)
	return err
}
