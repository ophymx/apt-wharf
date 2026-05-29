// Command chandler turns the vendor "curl URL | sudo tee
// /etc/apt/sources.list.d/foo.list" install ritual into a reproducible
// keyring + sources .deb. Discover-only; emits plan.Plan for `cooper
// build -` to consume.
//
// See chandler-design.md for the full design.
//
// Naming: a chandler historically supplied ships and ports with
// provisions — the trust material a voyage required. apt-chandler
// supplies systems with the trust material (signing keys) and routing
// information (.sources files) they need to reach third-party apt repos.
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
		fmt.Println(version.String("chandler"))
		return
	default:
		fmt.Fprintf(os.Stderr, "chandler: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "chandler:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  chandler validate <CONFIG>")
	fmt.Fprintln(w, "  chandler discover <CONFIG> [-o OUTPUT]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Pipe discover output into `cooper build -` to produce .debs:")
	fmt.Fprintln(w, "  chandler discover ./chandler.yaml | cooper build -")
}
