package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/staves/config"
	"github.com/ophymx/apt-wharf/internal/staves/discover"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

const stavesVersion = "0.1.0"

// stringSlice is the standard `--flag value` repeatable-string idiom.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// cmdDiscover implements `staves discover <CONFIG>`. Reads staves.yaml,
// walks each package directory, packs local files into aux_files,
// derives source_date_epoch from git, and emits a plan.Plan JSON
// document on stdout (or -o OUTPUT).
//
// Output is compatible with `cooper build -`; pipe the two together
// to build .debs end-to-end.
func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: staves discover <CONFIG> [--package NAME...] [-o OUTPUT]")
	}
	var packageFilter stringSlice
	fs.Var(&packageFilter, "package", "restrict to package (repeatable)")
	out := fs.String("o", "-", "output path (`-` for stdout)")
	allowShallow := fs.Bool("allow-shallow", false, "bypass the shallow-clone safety gate (source_date_epoch derived from `git log` is unreliable in shallow checkouts; pass this only for one-off local builds where you know HEAD is the relevant commit)")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional CONFIG argument")
	}

	top, err := config.LoadTop(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("load top config: %w", err)
	}

	var filter map[string]bool
	if len(packageFilter) > 0 {
		filter = make(map[string]bool, len(packageFilter))
		for _, name := range packageFilter {
			filter[name] = true
		}
	}

	p, err := discover.Run(context.Background(), top, discover.Options{
		Tool: plan.Tool{
			Name:           "staves",
			Version:        stavesVersion,
			FormatRevision: plan.FormatRevision,
		},
		PackageFilter: filter,
		AllowShallow:  *allowShallow,
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
