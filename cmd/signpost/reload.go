package main

import (
	"errors"
	"fmt"

	"github.com/ophymx/apt-wharf/internal/signpost/config"
)

// assertHotReloadable rejects reloads that would change fields the running
// daemon can't safely re-apply without restart:
//
//   - server.listen — would require tearing down and rebinding the listener.
//   - paths.state_dir — switching mid-flight orphans the state files written
//     under the old path and risks losing per-source provenance.
//
// All other fields (sources, signing.key_file, refresh.*, repository.*,
// suite.*, bootstrap.*, github.*) are safe to swap on the next tick: the
// rebuilt Wired re-loads secrets and the signing key, the tracker reseeds
// from disk, and the next refresh tick uses the new values.
//
// On rejection the caller should log the error and keep the prior config
// running — a failed reload is never grounds for crashing.
func assertHotReloadable(prev, next *config.Config) error {
	var errs []error
	if prev.Server.Listen != next.Server.Listen {
		errs = append(errs, fmt.Errorf(
			"server.listen changed (%q → %q); restart required to rebind",
			prev.Server.Listen, next.Server.Listen))
	}
	if prev.Paths.StateDir != next.Paths.StateDir {
		errs = append(errs, fmt.Errorf(
			"paths.state_dir changed (%q → %q); restart required to avoid orphaning state",
			prev.Paths.StateDir, next.Paths.StateDir))
	}
	return errors.Join(errs...)
}
