// Command staves packages locally-checked-in files (configs, systemd
// units, scripts, maintainer scripts — the kind of payload that has
// no upstream release to track) into Debian .deb files via cooper's
// build pipeline.
//
// Staves owns the discover side only: walk a package directory, pack
// every referenced file into aux_files, derive source_date_epoch from
// git commit time, and emit a plan.Plan compatible with `cooper build`.
// The build subcommand is cooper's — that pipeline accepts plans with
// Asset.URL=="" and skips the download/extract phase. See
// cooper-design.md §"Source kinds" and the staves design doc.
//
// Naming: a barrel's staves are the wooden planks that cooper
// assembles into the finished container. The same metaphor: cooper
// fetches and shapes upstream binaries, staves contributes the planks
// already on hand.
package main

import (
	"fmt"
	"os"

	"github.com/ophymx/apt-wharf/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "discover":
		err = cmdDiscover(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "-v", "--version", "version":
		fmt.Println(version.String("staves"))
		return
	default:
		fmt.Fprintf(os.Stderr, "staves: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "staves:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  staves validate <CONFIG>")
	fmt.Fprintln(w, "  staves discover <CONFIG> [--package NAME...] [-o OUTPUT]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Pipe discover output into `cooper build -` to produce .debs:")
	fmt.Fprintln(w, "  staves discover ./staves.yaml | cooper build -")
}
