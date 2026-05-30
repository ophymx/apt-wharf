package build

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// NfpmExecutor is the function shape the build orchestrator calls to
// produce a .deb. The default implementation execs the real nfpm
// binary; tests substitute a stub that fabricates a .deb on disk so
// they can run without nfpm installed.
type NfpmExecutor func(ctx context.Context, stagingDir, outputPath string, env []string) error

// DefaultNfpmExec runs `nfpm pkg --packager deb -f nfpm.yaml -t <outputPath>`
// with cwd=stagingDir and env restricted to the scrubbed set per
// cooper-design.md §"Reproducibility".
func DefaultNfpmExec(ctx context.Context, stagingDir, outputPath string, env []string) error {
	cmd := exec.CommandContext(ctx, "nfpm", "pkg",
		"--packager", "deb",
		"-f", "nfpm.yaml",
		"-t", outputPath,
	)
	cmd.Dir = stagingDir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nfpm pkg failed: %w\n--- nfpm output ---\n%s\n--- end nfpm output ---", err, out)
	}
	return nil
}

// BuildEnv returns the exact env vars cooper exposes to nfpm. Anything
// inherited from the parent process is dropped — only this set survives.
// PATH is fixed to a minimal set; the user's nfpm install must live
// under one of these directories.
//
// sourceDir is the absolute path to the extracted source_archive (when
// the artifact has one); pass "" to omit the SOURCE env var entirely
// for recipes that don't opt into source_archive. Recipes that
// reference ${SOURCE} without enabling source_archive will fail nfpm
// expansion (intentional — the missing var surfaces the configuration
// gap loudly instead of producing a .deb with an unexpanded literal).
func BuildEnv(version, arch, assetsDir, sourceDir string, sourceDateEpoch int64) []string {
	env := []string{
		"VERSION=" + version,
		"ARCH=" + arch,
		"ASSETS=" + assetsDir,
		"SOURCE_DATE_EPOCH=" + strconv.FormatInt(sourceDateEpoch, 10),
		"LC_ALL=C",
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	if sourceDir != "" {
		env = append(env, "SOURCE="+sourceDir)
	}
	return env
}
