// Command drayman is the orchestrator that hauls cooper's build output
// into an apt repository (aptly). drayman reads a cooper discover JSON
// plan, queries the target repo for already-imported
// X-Cooper-Build-Inputs-Hash values, auto-bumps debian-revisions for
// artifacts whose (Package, base-version, Architecture) already exists
// with a different hash, exec's cooper build on the surviving subset,
// uploads the resulting .debs to aptly, and triggers a publish update.
//
// See cooper-design.md §"Orchestrator dedup & version policy" for the
// algorithm. drayman is the third tool in the apt-signpost / apt-cooper
// / apt-drayman trio: signpost serves redirected metadata, cooper
// builds reproducible .debs, drayman hauls them into the repo.
package main

import (
	"fmt"
	"os"
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
	fmt.Fprintln(w, "  drayman reconcile <PLAN_FILE> --aptly-url URL --repo NAME [flags]")
	fmt.Fprintln(w, "  drayman peek      <PLAN_FILE> --aptly-url URL --repo NAME")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "reconcile flags:")
	fmt.Fprintln(w, "  --out-dir DIR             where cooper builds .debs (default: ephemeral)")
	fmt.Fprintln(w, "  --cooper-bin PATH         path to cooper binary (default: cooper)")
	fmt.Fprintln(w, "  --publish-prefix STRING   aptly publish prefix (default: .)")
	fmt.Fprintln(w, "  --publish-distribution S  aptly publish distribution (default: repo's default)")
	fmt.Fprintln(w, "  --dry-run                 query and decide but don't build, upload, or publish")
}
