// Command drayman is the orchestrator that hauls cooper's build output
// into an apt repository (aptly). drayman reads a cooper discover JSON
// plan, queries the target repo for already-imported
// X-Cooper-Build-Inputs-Hash values, auto-bumps debian-revisions for
// artifacts whose (Package, base-version, Architecture) already exists
// with a different hash, exec's cooper build on the surviving subset,
// uploads the resulting .debs to aptly, optionally prunes versions
// beyond --retain N, and triggers a publish update.
//
// Beside that reconcile flow (and its read-only preview, peek),
// `import` hauls .debs that something other than cooper built, `has`
// answers whether the repo already carries a given package version,
// and `publish` regenerates the published metadata on its own.
//
// See cooper-design.md §"Orchestrator dedup & version policy" for the
// algorithm. drayman is the hauler in the apt-wharf family: cooper /
// chandler / staves produce reproducible .debs, drayman hauls them
// into the target apt repo (aptly or reprepro).
package main

import (
	"errors"
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
	case "import":
		err = cmdImport(os.Args[2:])
	case "has":
		err = cmdHas(os.Args[2:])
	case "publish":
		err = cmdPublish(os.Args[2:])
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
		if ec, ok := errors.AsType[exitCodeError](err); ok {
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "drayman:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  drayman reconcile <PLAN_FILE> --backend <kind> [backend flags] [--retain N]")
	fmt.Fprintln(w, "  drayman peek      <PLAN_FILE> --backend <kind> [backend flags] [--retain N]")
	fmt.Fprintln(w, "  drayman import    <DEB>...    --backend <kind> [backend flags] [--retain N]")
	fmt.Fprintln(w, "  drayman has       --name NAME --version VERSION [--arch ARCH] --backend <kind> [backend flags]")
	fmt.Fprintln(w, "  drayman publish   --backend <kind> [backend flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  reconcile  build what a cooper/chandler/staves plan adds and import it")
	fmt.Fprintln(w, "  peek       print the decisions reconcile would make; changes nothing")
	fmt.Fprintln(w, "  import     import .debs built outside cooper, skipping versions already in the repo")
	fmt.Fprintln(w, "  has        exit 0 if the repo has that version, 10 if not, 1 if it couldn't tell")
	fmt.Fprintln(w, "  publish    regenerate published metadata only (retry after a failed publish)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "backends:")
	fmt.Fprintln(w, "  aptly           --aptly-url URL --repo NAME [--publish-prefix .]")
	fmt.Fprintln(w, "                  --publish-distribution DIST [--skip-signing]")
	fmt.Fprintln(w, "                  [--publish-architectures amd64,arm64]  for the first publish of a new")
	fmt.Fprintln(w, "                                        distribution holding only arch:all packages")
	fmt.Fprintln(w, "                  [--aptly-timeout 5m]  per-request wait; uploads are bounded by progress")
	fmt.Fprintln(w, "                  [--aptly-db-cleanup]  after a run that pruned, drop unreferenced pool")
	fmt.Fprintln(w, "                                        files (acts on the whole aptly instance)")
	fmt.Fprintln(w, "  reprepro-local  --reprepro-basedir DIR --distribution DIST [--component main]")
	fmt.Fprintln(w, "  reprepro-ssh    --ssh-dest user@host --reprepro-basedir DIR")
	fmt.Fprintln(w, "                  --distribution DIST [--component main]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "reconcile flags:")
	fmt.Fprintln(w, "  --cooper-bin PATH     path to cooper binary (default: cooper)")
	fmt.Fprintln(w, "  --out-dir DIR         where cooper builds .debs (default: ephemeral)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "shared by reconcile and import:")
	fmt.Fprintln(w, "  --dry-run             query and decide but don't build, import, prune, or publish")
	fmt.Fprintln(w, "  --audit-log PATH      append JSONL audit events (decisions/imports/prunes/publish/cleanup)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "shared by reconcile, peek, and import:")
	fmt.Fprintln(w, "  --retain N            keep only the N newest upstream versions per (package, arch),")
	fmt.Fprintln(w, "                        newest revision each; prunes the rest (default 0 = keep all).")
	fmt.Fprintln(w, "                        reconcile/peek only ever prune cooper-built entries; import")
	fmt.Fprintln(w, "                        prunes any entry of the (package, arch) pairs it was handed")
}
