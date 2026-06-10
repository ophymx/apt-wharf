package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ophymx/apt-wharf/internal/cli"
	"github.com/ophymx/apt-wharf/internal/cooper/config"
	"github.com/ophymx/apt-wharf/internal/cooper/stage"
)

// cmdValidate implements `cooper validate <CONFIG>` per cooper-design.md
// §"CLI". Lints without network: parses cooper.yaml, walks each package's
// multi-doc file, runs aux-file resolution, and renders every .tmpl
// against placeholder Vars to surface execute-time errors.
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: cooper validate <CONFIG>")
	}
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

	anyFail := false
	for _, pkgPath := range top.Packages {
		ok, label, msg := validateOne(pkgPath)
		mark := "ok"
		if !ok {
			mark = "FAIL"
			anyFail = true
		}
		fmt.Printf("%-4s  %-30s  %s\n", mark, label, msg)
	}
	if anyFail {
		return errors.New("one or more packages failed validation")
	}
	return nil
}

// validateOne runs the full lint pipeline for a single recipe file
// (one cooper sidecar + N nfpm docs). label is the first nfpm doc's
// name (or the file's basename if loading failed before we knew it);
// msg is a short status string aggregated across every nfpm doc.
func validateOne(pkgPath string) (ok bool, label, msg string) {
	pkg, err := config.LoadPackage(pkgPath)
	if err != nil {
		return false, filepath.Base(pkgPath), fmt.Sprintf("config: %v", err)
	}
	label = pkg.NfpmDocs[0].Name
	totalRefs, totalTmpls := 0, 0
	for _, doc := range pkg.NfpmDocs {
		refs, werr := stage.Walk(pkg.Dir, &doc.Node)
		if werr != nil {
			return false, doc.Name, fmt.Sprintf("aux (%s): %v", doc.Name, werr)
		}
		totalRefs += len(refs)
		for _, r := range refs {
			if !r.Template {
				continue
			}
			body, rerr := os.ReadFile(r.AbsPath)
			if rerr != nil {
				return false, doc.Name, fmt.Sprintf("read %s: %v", r.AbsPath, rerr)
			}
			if _, terr := stage.RenderTemplate(r.Key, body, stage.PlaceholderVars()); terr != nil {
				return false, doc.Name, fmt.Sprintf("template %s: %v", r.Key, terr)
			}
			totalTmpls++
		}
		// Substitution coverage per arch: build the same subs map
		// discover would use (with a placeholder VERSION since
		// validate is offline) and run a dry substitution to surface
		// arch_gnu_unknown / unresolved_substitution errors before
		// any release fetch.
		for arch, archCfg := range pkg.Sidecar.Arches {
			clone, cerr := stage.CloneNfpm(&doc.Node)
			if cerr != nil {
				return false, doc.Name, fmt.Sprintf("clone (%s/%s): %v", doc.Name, arch, cerr)
			}
			subs := stage.BuildSubs(stage.PlaceholderVars().Version, arch, archCfg.Vars)
			if serr := stage.SubstituteNfpm(clone, arch, subs); serr != nil {
				return false, doc.Name, fmt.Sprintf("substitute (%s/%s):\n%v", doc.Name, arch, serr)
			}
		}
	}
	if len(pkg.NfpmDocs) == 1 {
		return true, label, fmt.Sprintf("%d arches, %d aux refs (%d templates)",
			len(pkg.Sidecar.Arches), totalRefs, totalTmpls)
	}
	names := make([]string, len(pkg.NfpmDocs))
	for i, d := range pkg.NfpmDocs {
		names[i] = d.Name
	}
	return true, label, fmt.Sprintf("%d arches, %d nfpm docs [%s], %d aux refs (%d templates)",
		len(pkg.Sidecar.Arches), len(pkg.NfpmDocs), strings.Join(names, ", "), totalRefs, totalTmpls)
}
