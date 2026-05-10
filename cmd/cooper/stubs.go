package main

import "errors"

// cmdBuild is wired in a later slice. Stubbed to keep `cooper --help`
// accurate without prematurely committing CLI surface for the unfinished
// phase.

func cmdBuild(args []string) error {
	return errors.New("build: not implemented yet")
}
