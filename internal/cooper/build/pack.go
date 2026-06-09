package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/goreleaser/nfpm/v2"

	// Register the deb packager so nfpm.Get("deb") resolves.
	_ "github.com/goreleaser/nfpm/v2/deb"
)

// PackInputs carries the per-artifact context Pack hands to nfpm. It
// replaces the env slice the prior exec-based path passed via cmd.Env:
// the same five variables (VERSION, ARCH, ASSETS, SOURCE_DATE_EPOCH,
// SOURCE) flow through the env-mapping function plugged into
// nfpm.ParseWithEnvMapping, and MTime is pinned explicitly so nfpm
// never falls back to os.Getenv("SOURCE_DATE_EPOCH").
type PackInputs struct {
	Version         string
	Arch            string
	AssetsDir       string
	SourceDir       string // "" when the artifact has no source_archive
	SourceDateEpoch int64
}

// envMapping returns the func(string) string that nfpm's yaml-parse
// loop uses to resolve ${VAR} references in the staged nfpm doc. Only
// the cooper-controlled vars resolve; everything else returns "", which
// matches the behavior of the prior exec path running under a scrubbed
// env (os.Expand on an unset name returns "").
func (in PackInputs) envMapping() func(string) string {
	env := map[string]string{
		"VERSION":           in.Version,
		"ARCH":              in.Arch,
		"ASSETS":            in.AssetsDir,
		"SOURCE_DATE_EPOCH": strconv.FormatInt(in.SourceDateEpoch, 10),
	}
	if in.SourceDir != "" {
		env["SOURCE"] = in.SourceDir
	}
	return func(name string) string { return env[name] }
}

// Pack reads <stagingDir>/nfpm.yaml, builds the .deb in-process via
// nfpm/v2, and writes it to outputPath. Replaces the prior
// `exec("nfpm pkg ...")` path; the staged yaml stays on disk
// unchanged so --keep-work / --stage-only inspection works the same.
//
// Three properties matter for reproducibility:
//
//   - The env mapping is cooper-controlled (PackInputs), not
//     os.Getenv — process env never leaks into ${VAR} expansion.
//   - info.MTime is pinned to in.SourceDateEpoch before validation;
//     nfpm's WithDefaults would otherwise call modtime.FromEnv() which
//     reads os.Getenv("SOURCE_DATE_EPOCH") directly.
//   - Relative `src:` paths in contents resolve against stagingDir,
//     not the process cwd, mirroring the prior cmd.Dir=staging exec.
func Pack(_ context.Context, stagingDir, outputPath string, in PackInputs) error {
	f, err := os.Open(filepath.Join(stagingDir, "nfpm.yaml"))
	if err != nil {
		return fmt.Errorf("open nfpm.yaml: %w", err)
	}
	defer f.Close()

	config, err := nfpm.ParseWithEnvMapping(f, in.envMapping())
	if err != nil {
		return fmt.Errorf("parse nfpm.yaml: %w", err)
	}
	info, err := config.Get("deb")
	if err != nil {
		return fmt.Errorf("nfpm config.Get(deb): %w", err)
	}
	info.Target = outputPath
	info.MTime = time.Unix(in.SourceDateEpoch, 0).UTC()
	absolutizeStagedPaths(info, stagingDir)

	nfpm.WithDefaults(info)
	if err := nfpm.Validate(info); err != nil {
		return fmt.Errorf("nfpm validate: %w", err)
	}

	packager, err := nfpm.Get("deb")
	if err != nil {
		return fmt.Errorf("nfpm.Get(deb): %w", err)
	}

	out, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	if err := packager.Package(info, out); err != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("nfpm package: %w", err)
	}
	return nil
}

// absolutizeStagedPaths rewrites every relative file-reference in the
// in-scope nfpm fields to be rooted at stagingDir. The prior exec
// path achieved the same thing implicitly via cmd.Dir = staging; with
// the library call, nfpm's os.Stat / file reads use the process cwd,
// so we resolve the paths here. Absolute paths (the common case after
// ${ASSETS}/${SOURCE} expansion) are left alone.
//
// In-scope fields match cooper-design.md's "Aux file resolution"
// list: contents[].src plus the four scripts.{pre,post}{install,remove}
// entries. Other file-referencing fields (changelog, deb.scripts.*,
// rpm.scripts.*, signature.key_file, ...) are passthrough per the
// design — cooper doesn't stage files for them and the user is on the
// hook to use absolute paths.
func absolutizeStagedPaths(info *nfpm.Info, stagingDir string) {
	for _, c := range info.Contents {
		if c.Source != "" && !filepath.IsAbs(c.Source) {
			c.Source = filepath.Join(stagingDir, c.Source)
		}
	}
	for _, p := range []*string{
		&info.Scripts.PreInstall,
		&info.Scripts.PostInstall,
		&info.Scripts.PreRemove,
		&info.Scripts.PostRemove,
	} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(stagingDir, *p)
		}
	}
}
