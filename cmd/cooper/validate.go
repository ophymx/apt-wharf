package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
	"github.com/ophymx/apt-signpost/internal/cooper/stage"
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
	if err := fs.Parse(args); err != nil {
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

// validateOne runs the full lint pipeline for a single package file.
// label is the package's name (or the file's basename if loading failed
// before we knew the name); msg is a short status string.
func validateOne(pkgPath string) (ok bool, label, msg string) {
	pkg, err := config.LoadPackage(pkgPath)
	if err != nil {
		return false, filepath.Base(pkgPath), fmt.Sprintf("config: %v", err)
	}
	refs, err := stage.Walk(pkg.Dir, &pkg.Nfpm)
	if err != nil {
		return false, pkg.NfpmName, fmt.Sprintf("aux: %v", err)
	}
	tmplCount := 0
	for _, r := range refs {
		if !r.Template {
			continue
		}
		body, err := os.ReadFile(r.AbsPath)
		if err != nil {
			return false, pkg.NfpmName, fmt.Sprintf("read %s: %v", r.AbsPath, err)
		}
		if _, err := stage.RenderTemplate(r.Key, body, stage.PlaceholderVars()); err != nil {
			return false, pkg.NfpmName, fmt.Sprintf("template %s: %v", r.Key, err)
		}
		tmplCount++
	}
	return true, pkg.NfpmName, fmt.Sprintf("%d arches, %d aux refs (%d templates)",
		len(pkg.Sidecar.Arches), len(refs), tmplCount)
}
