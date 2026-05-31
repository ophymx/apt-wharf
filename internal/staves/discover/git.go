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

// IsShallowRepo reports whether the working tree at dir is a shallow
// git checkout (typical CI pattern: `git fetch --depth=1`). In a
// shallow clone `git log -- <path>` returns HEAD's timestamp
// regardless of whether HEAD actually touched <path>, which breaks
// source_date_epoch stability and forces drayman to bump the debian
// revision on every push. Discover callers gate the rest of the
// pipeline on this so the failure surfaces loudly instead of
// poisoning the fleet's build_inputs_hash values silently.
func IsShallowRepo(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-shallow-repository")
	out, err := cmd.Output()
	if err != nil {
		// Treat any failure (not a repo, git missing, etc.) as
		// "shallow status unknown" rather than gating. The
		// GitProvenance call that follows will surface the real
		// problem with its own actionable message ("not in git
		// working tree" / "no commits touch this path"); blocking
		// here would replace those with a vaguer rev-parse error.
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// GitProvenance returns the commit hash and commit time of the most
// recent commit that touched any file under dir. Used to derive
// source_date_epoch for staves packages, since there's no upstream
// "published_at" to lean on.
//
// Errors when dir isn't inside a git working tree, or when no commits
// touch the package directory yet (typical for a freshly-added
// package — the operator must `git add` and commit before discover).
func GitProvenance(ctx context.Context, dir string) (commit string, when time.Time, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "-n", "1", "--format=%H%n%ct", "--", ".")
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", time.Time{}, fmt.Errorf("git log -- . in %s: %w", dir, runErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("git log in %s: no commits touch this package (run `git add %s && git commit` first)", dir, dir)
	}
	commit = parts[0]
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git log in %s: parse commit timestamp %q: %w", dir, parts[1], err)
	}
	return commit, time.Unix(ts, 0).UTC(), nil
}
