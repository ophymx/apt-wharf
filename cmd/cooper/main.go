// Command cooper turns GitHub-released binaries into Debian .deb files.
//
// See cooper-design.md for the full design. This binary implements
// cooper's two phases (discover, build) plus a network-free validate
// linter.
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
	case "build":
		err = cmdBuild(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "-v", "--version", "version":
		fmt.Println(version.String("cooper"))
		return
	default:
		fmt.Fprintf(os.Stderr, "cooper: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cooper:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  cooper validate <CONFIG>")
	fmt.Fprintln(w, "  cooper discover <CONFIG> [--package NAME...] [-o OUTPUT]")
	fmt.Fprintln(w, "  cooper build    <JSON_FILE> [--out-dir DIR] [--work-dir DIR]")
	fmt.Fprintln(w, "                              [--stage-only | --keep-work]")
}
