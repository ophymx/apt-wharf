package main

import "errors"

// cmdDiscover and cmdBuild are wired in later slices. Stubbed to keep
// `cooper --help` accurate without prematurely committing CLI surface
// for the unfinished phases.

func cmdDiscover(args []string) error {
	return errors.New("discover: not implemented yet")
}

func cmdBuild(args []string) error {
	return errors.New("build: not implemented yet")
}
