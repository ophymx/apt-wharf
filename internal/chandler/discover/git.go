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
// recent commit that touched the chandler config file. Used to derive
// source_date_epoch — chandler has no upstream "published_at" to lean
// on, so the config file's own history is the deterministic clock.
//
// Errors when the file isn't tracked in a git working tree, or when no
// commits touch it yet (typical for a freshly-added config — the
// operator must `git add` and commit before discover). This is the
// chandler-design.md "not-in-git is a hard error" rule.
func gitProvenance(ctx context.Context, configPath string) (commit string, when time.Time, err error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-n", "1", "--format=%H%n%ct", "--", configPath)
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", time.Time{}, fmt.Errorf("git log -- %s: %w", configPath, runErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("git log %s: no commit touches this file (run `git add %s && git commit` first)", configPath, configPath)
	}
	commit = parts[0]
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git log %s: parse commit timestamp %q: %w", configPath, parts[1], err)
	}
	return commit, time.Unix(ts, 0).UTC(), nil
}
