package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ophymx/apt-wharf/internal/chandler/config"
)

// cmdValidate implements `chandler validate <CONFIG>`. Lints without
// network: parses chandler.yaml under strict-mode YAML decoding,
// verifies key references resolve, source IDs are unique and
// filename-safe, and (in matrix mode) the package.name carries a
// template token. See chandler-design.md "CLI · validate".
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: chandler validate <CONFIG>")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one positional CONFIG argument")
	}

	cfg, err := config.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := config.ValidateMatrix(cfg); err != nil {
		return err
	}

	mode := "simple"
	if len(cfg.Targets) > 1 {
		mode = fmt.Sprintf("matrix (%d targets)", len(cfg.Targets))
	} else if len(cfg.Targets) == 1 {
		mode = "matrix (1 target)"
	}
	fmt.Printf("ok    %-40s  keys=%d sources=%d mode=%s\n",
		cfg.Package.Name, len(cfg.Keys), len(cfg.Sources), mode)
	return nil
}
