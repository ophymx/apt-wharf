// Command drayman is the orchestrator that hauls cooper's build output
// into an apt repository (aptly). drayman reads a cooper discover JSON
// plan, queries the target repo for already-imported
// X-Cooper-Build-Inputs-Hash values, auto-bumps debian-revisions for
// artifacts whose (Package, base-version, Architecture) already exists
// with a different hash, exec's cooper build on the surviving subset,
// uploads the resulting .debs to aptly, and triggers a publish update.
//
// See cooper-design.md §"Orchestrator dedup & version policy" for the
// algorithm. drayman is the hauler in the apt-wharf family: cooper /
// chandler / staves produce reproducible .debs, drayman hauls them
// into the target apt repo (aptly or reprepro).
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
	case "reconcile":
		err = cmdReconcile(os.Args[2:])
	case "peek":
		err = cmdPeek(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "-v", "--version", "version":
		fmt.Println(version.String("drayman"))
		return
	default:
		fmt.Fprintf(os.Stderr, "drayman: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "drayman:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  drayman reconcile <PLAN_FILE> --backend <kind> [backend flags]")
	fmt.Fprintln(w, "  drayman peek      <PLAN_FILE> --backend <kind> [backend flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "backends:")
	fmt.Fprintln(w, "  aptly           --aptly-url URL --repo NAME [--publish-prefix .]")
	fmt.Fprintln(w, "                  --publish-distribution DIST [--skip-signing]")
	fmt.Fprintln(w, "  reprepro-local  --reprepro-basedir DIR --distribution DIST [--component main]")
	fmt.Fprintln(w, "  reprepro-ssh    --ssh-dest user@host --reprepro-basedir DIR")
	fmt.Fprintln(w, "                  --distribution DIST [--component main]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "reconcile common flags:")
	fmt.Fprintln(w, "  --cooper-bin PATH     path to cooper binary (default: cooper)")
	fmt.Fprintln(w, "  --out-dir DIR         where cooper builds .debs (default: ephemeral)")
	fmt.Fprintln(w, "  --dry-run             query and decide but don't build, import, or publish")
	fmt.Fprintln(w, "  --audit-log PATH      append JSONL audit events (decisions/imports/publish)")
}
