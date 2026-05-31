// Package discover walks staves packages and emits a plan.Plan
// document ready for `cooper build -`. Pure local pipeline — no
// network, no upstream resolution.
package discover

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GitProvenance returns the commit hash and commit time of the most
// recent commit that touched any file under dir. The result is
// surfaced as best-effort provenance (GitCommit / GitDate fields on
// plan.Source) — source_date_epoch is now derived from recipe content
// in run.go, so a missing-git or no-touching-commit error here is
// swallowed by the caller and the resulting plan simply omits the
// provenance fields.
//
// Returns a non-nil error when dir isn't inside a git working tree or
// no commit touches the package directory yet. Callers ignore the
// error and check for empty commit/when instead.
func GitProvenance(ctx context.Context, dir string) (commit string, when time.Time, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "-n", "1", "--format=%H%n%ct", "--", ".")
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", time.Time{}, fmt.Errorf("git log -- . in %s: %w", dir, runErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("git log in %s: no commits touch this package yet", dir)
	}
	commit = parts[0]
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git log in %s: parse commit timestamp %q: %w", dir, parts[1], err)
	}
	return commit, time.Unix(ts, 0).UTC(), nil
}
