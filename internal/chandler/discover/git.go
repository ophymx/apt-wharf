package discover

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitProvenance returns the commit hash and commit time of the most
// recent commit that touched the chandler config file. The result is
// surfaced as best-effort provenance (GitCommit / GitDate fields on
// plan.Source) — source_date_epoch is now derived from recipe content
// in run.go, so a missing-git or no-touching-commit error here is
// swallowed by the caller and the resulting plan simply omits the
// provenance fields.
//
// Returns a non-nil error when the file isn't in a git working tree or
// no commit touches it yet. Callers ignore the error and check for
// empty commit/when instead.
func gitProvenance(ctx context.Context, configPath string) (commit string, when time.Time, err error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-n", "1", "--format=%H%n%ct", "--", configPath)
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", time.Time{}, fmt.Errorf("git log -- %s: %w", configPath, runErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("git log %s: no commit touches this file yet", configPath)
	}
	commit = parts[0]
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git log %s: parse commit timestamp %q: %w", configPath, parts[1], err)
	}
	return commit, time.Unix(ts, 0).UTC(), nil
}
