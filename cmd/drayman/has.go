package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
	"github.com/ophymx/apt-wharf/pkg/plan"
)

// exitMissing is `drayman has`'s exit status for "queried fine, not
// in the repo". It is distinct from 1 (transport, auth, or usage
// error) so a recipe's pre-build check can tell "go build it" from
// "couldn't ask" — rebuilding on the latter would turn an outage
// into a rebuild loop.
const exitMissing = 10

// exitCodeError carries a specific process exit status up to main,
// which exits with it silently: the subcommand has already printed
// whatever the operator needs.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// cmdHas implements `drayman has --name N --version V [--arch A]` —
// a read-only check for whether the target repo already carries that
// package version. Exit 0 = present, 10 = missing, 1 = couldn't tell.
//
// It is the cheap idempotency anchor for packages built outside
// cooper (see `drayman import`): an expensive build asks this first
// instead of running just to find out it wasn't needed.
func cmdHas(args []string) error {
	fs := flag.NewFlagSet("has", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: drayman has --name NAME --version VERSION [--arch ARCH] --backend <kind> [backend flags]")
		fs.PrintDefaults()
	}
	bf := registerBackendFlags(fs)
	name := fs.String("name", "", "Debian package name (required)")
	version := fs.String("version", "", "full Debian version, compared the way dpkg does (required)")
	arch := fs.String("arch", "", "architecture; default: any")
	if err := fs.Parse(cli.ReorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *name == "" || *version == "" {
		fs.Usage()
		return errors.New("--name and --version are required")
	}
	be, err := bf.resolveBackend()
	if err != nil {
		return err
	}
	return runHas(context.Background(), be, *name, *version, *arch, os.Stdout)
}

// runHas prints one `<name>\t<arch>\tpresent\t<version>` row per
// matching repo entry, or a single `missing` row and exitMissing when
// there is none. arch == "" matches any architecture.
func runHas(ctx context.Context, q backend.Querier, name, version, arch string, stdout io.Writer) error {
	var pkgs []backend.Package
	var err error
	if arch != "" {
		pkgs, err = q.ListByNameArch(ctx, name, arch)
	} else {
		pkgs, err = q.ListByName(ctx, name)
	}
	if err != nil {
		return fmt.Errorf("query %s: %w", name, err)
	}

	// reprepro lists an arch:all entry once per binary-<arch> index.
	type row struct{ arch, version string }
	seen := map[row]bool{}
	for _, p := range pkgs {
		r := row{p.Architecture, p.Version}
		if plan.CompareVersions(p.Version, version) != 0 || seen[r] {
			continue
		}
		seen[r] = true
		fmt.Fprintf(stdout, "%s\t%s\tpresent\t%s\n", p.Name, p.Architecture, p.Version)
	}
	if len(seen) == 0 {
		if arch == "" {
			arch = "*"
		}
		fmt.Fprintf(stdout, "%s\t%s\tmissing\t%s\n", name, arch, version)
		return exitCodeError{exitMissing}
	}
	return nil
}
