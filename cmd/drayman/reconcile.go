package main

import "errors"

// cmdReconcile implements the full drayman flow: read plan, query
// aptly, decide per artifact, exec cooper build on the surviving
// subset, upload .debs to aptly, trigger publish. Wired up in a
// follow-up commit; for now `peek` is the end-to-end touchpoint that
// exercises the read paths and policy.
func cmdReconcile(_ []string) error {
	return errors.New("reconcile: not yet implemented (use `drayman peek` to inspect policy decisions)")
}
