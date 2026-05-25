package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ophymx/apt-wharf/internal/cooper/stage"
	"github.com/ophymx/apt-wharf/internal/staves/config"
)

// cmdValidate implements `staves validate <CONFIG>`. Lints without
// network: parses staves.yaml, walks each package's nfpm.yaml + files,
// rejects ${ASSETS}/${VAR} substitutions (no upstream asset in this
// pipeline), confirms every contents[].src resolves inside the
// package directory.
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: staves validate <CONFIG>")
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
	for _, pkgDir := range top.Packages {
		ok, label, msg := validateOne(pkgDir)
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

func validateOne(pkgDir string) (ok bool, label, msg string) {
	pkg, err := config.LoadPackage(pkgDir)
	if err != nil {
		return false, filepath.Base(pkgDir), fmt.Sprintf("config: %v", err)
	}
	// Reject upstream-style paths up front — stage.Walk silently
	// skips them, but for staves they'd just blow up at nfpm exec.
	if err := config.RejectAssetishSrcs(config.CollectSrcPaths(&pkg.Nfpm)); err != nil {
		return false, pkg.NfpmName, err.Error()
	}
	refs, err := stage.Walk(pkg.Dir, &pkg.Nfpm)
	if err != nil {
		return false, pkg.NfpmName, fmt.Sprintf("aux walk: %v", err)
	}
	return true, pkg.NfpmName, fmt.Sprintf("version=%s arch=%s aux refs=%d", pkg.Version, pkg.Arch, len(refs))
}
