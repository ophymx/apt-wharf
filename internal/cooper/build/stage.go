package build

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ophymx/apt-wharf/pkg/plan"
)

// NfpmYAMLOpts carries the on-disk-only adjustments cooper applies when
// writing nfpm.yaml. None of these flow back into BuildPlan.Nfpm in
// JSON, so none perturb build_inputs_hash.
type NfpmYAMLOpts struct {
	// Hash is the X-Cooper-Build-Inputs-Hash to inject into
	// deb.fields. Empty = skip injection.
	Hash string
	// Revision, when > 0, appends "-N" to the version field. The
	// caller is responsible for ensuring the bare version contains
	// no "-" before invoking this (run.go validates upfront).
	Revision int
}

// WriteNfpmYAML decodes BuildPlan.Nfpm (json.RawMessage) into a generic
// map and writes a deterministic YAML representation at target.
// yaml.v3's default emit sorts mapping keys alphabetically, which makes
// the on-disk file byte-stable across runs of the same plan.
//
// Applies the on-disk-only adjustments cooper-design.md §"Build"
// step 4 lists:
//
//	(a) expand: true on every contents entry that doesn't already have
//	    it explicitly set.
//	(b) deb.fields["X-Cooper-Build-Inputs-Hash"] = opts.Hash.
//	(c) version field gets "-N" appended when opts.Revision > 0.
//	(d) version_schema: none when the recipe didn't pick one, so nfpm
//	    doesn't rewrite the resolved version (semver padding,
//	    prerelease tildes) and the deb's Version: matches what cooper
//	    planned.
func WriteNfpmYAML(bp plan.BuildPlan, opts NfpmYAMLOpts, target string) error {
	var doc any
	if err := json.Unmarshal(bp.Nfpm, &doc); err != nil {
		return fmt.Errorf("decode nfpm json: %w", err)
	}
	doc = enableContentsExpand(doc)
	doc = injectBuildInputsHash(doc, opts.Hash)
	doc = applyRevision(doc, opts.Revision)
	doc = pinVersionSchema(doc)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal nfpm yaml: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, out, 0o644)
}

// applyRevision sets the nfpm `release` field to <revision> when
// revision > 0; nfpm's deb packager concatenates it as "-<release>"
// onto the version field, so the resulting Debian Version is
// "<version>-<release>". No-op otherwise.
//
// We use nfpm's dedicated `release` field rather than rewriting
// `version` so the JSON `build_plan.nfpm.version` stays revision-free
// — the same Plan re-fed with a different --revision is deterministic
// without the discover-time hash needing to know the revision.
// (pinVersionSchema separately keeps nfpm from rewriting "-N" suffixes
// into semver tilde-prereleases.)
func applyRevision(doc any, revision int) any {
	if revision <= 0 {
		return doc
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return doc
	}
	root["release"] = strconv.Itoa(revision)
	return root
}

// injectBuildInputsHash sets deb.fields["X-Cooper-Build-Inputs-Hash"]
// = hash on the nfpm doc, creating the `deb` and `deb.fields` maps if
// absent. No-op when hash is empty (defense in depth for callers that
// don't have a hash to inject, e.g. unit tests of unrelated adjustments).
func injectBuildInputsHash(doc any, hash string) any {
	if hash == "" {
		return doc
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return doc
	}
	deb, ok := root["deb"].(map[string]any)
	if !ok {
		deb = map[string]any{}
		root["deb"] = deb
	}
	fields, ok := deb["fields"].(map[string]any)
	if !ok {
		fields = map[string]any{}
		deb["fields"] = fields
	}
	fields["X-Cooper-Build-Inputs-Hash"] = hash
	return root
}

// pinVersionSchema sets `version_schema: none` on the nfpm doc when the
// recipe didn't pick a schema. nfpm's default `version_schema: semver`
// rewrites the resolved version in two ways that drift it away from
// what cooper planned:
//
//   - Pads short versions to three components (`4.32` → `4.32.0`), so
//     the deb's Version: control field disagrees with the filename
//     cooper writes (`<name>_<version>_<arch>.deb`) and with any
//     orchestrator that re-queries the repo for the current max
//     version. drayman's `(Package, base-version, Architecture)`
//     revision-slot lookup can't recover — every fresh build of the
//     same upstream tag plans the un-padded version while the repo
//     reports the padded one, an unbreakable version_regression skip.
//   - Re-emits a "-N" suffix as a tilde-prerelease ("~N"). cooper's
//     applyRevision already routes around this via the dedicated
//     release: field for orchestrator-driven bumps, but a recipe-baked
//     version_template can still trip it.
//
// Recipes that genuinely want semver behavior keep their explicit
// override.
func pinVersionSchema(doc any) any {
	root, ok := doc.(map[string]any)
	if !ok {
		return doc
	}
	if _, set := root["version_schema"]; set {
		return doc
	}
	root["version_schema"] = "none"
	return root
}

// enableContentsExpand walks the nfpm doc, finds the contents:[] list,
// and sets expand: true on every entry that doesn't explicitly opt out.
// Returns the (possibly mutated) doc.
func enableContentsExpand(doc any) any {
	root, ok := doc.(map[string]any)
	if !ok {
		return doc
	}
	contents, ok := root["contents"].([]any)
	if !ok {
		return doc
	}
	for i, entry := range contents {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if _, set := m["expand"]; !set {
			m["expand"] = true
		}
		contents[i] = m
	}
	return root
}

// WriteAuxFiles materializes every aux_files entry under stagingDir at
// its source-relative key. The keys are paths the user wrote in doc 2
// (e.g. "./hugo.service") so nfpm's relative-path resolution works
// naturally with cwd=stagingDir.
func WriteAuxFiles(aux map[string]plan.AuxFile, stagingDir string) error {
	absStaging, err := filepath.Abs(stagingDir)
	if err != nil {
		return err
	}
	absStaging = filepath.Clean(absStaging)

	for key, file := range aux {
		body, err := base64.StdEncoding.DecodeString(file.ContentB64)
		if err != nil {
			return fmt.Errorf("aux %s: decode b64: %w", key, err)
		}
		// Lexically resolve the key against stagingDir; key shapes are
		// produced by stage.Walk and don't contain ${VAR}/absolute paths,
		// but containment is still asserted to defend against tampered
		// plans.
		target := filepath.Join(absStaging, filepath.FromSlash(key))
		if !pathInside(absStaging, target) {
			return fmt.Errorf("aux %s resolves outside staging dir", key)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("aux %s: mkdir parent: %w", key, err)
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("aux %s: write: %w", key, err)
		}
	}
	return nil
}

// PinMtimes walks stagingDir and rewrites every entry's atime and mtime
// to source_date_epoch. nfpm reads each src file's mtime and copies it
// into the inner ar/tar entries; pinning here is what makes the
// resulting .deb byte-identical across runs of the same JSON plan.
//
// Symlinks are not retimed (os.Chtimes follows them, so a non-existent
// target would error). The targets themselves get pinned via the regular
// walk; nfpm only cares about target mtimes.
func PinMtimes(stagingDir string, sourceDateEpoch int64) error {
	t := time.Unix(sourceDateEpoch, 0).UTC()
	return filepath.WalkDir(stagingDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return os.Chtimes(p, t, t)
	})
}

// RejectNonZeroOwners scans BuildPlan.Nfpm.contents[] and rejects any
// entry whose file_info.owner or file_info.group is set to a value
// other than "", "0", or "root". cooper-design.md §"Reproducibility"
// requires 0/0 ownership on every entry; allowing user overrides would
// break the byte-identical guarantee.
func RejectNonZeroOwners(bp plan.BuildPlan) error {
	var nfpm map[string]any
	if err := json.Unmarshal(bp.Nfpm, &nfpm); err != nil {
		return fmt.Errorf("decode nfpm json: %w", err)
	}
	contents, _ := nfpm["contents"].([]any)
	for i, c := range contents {
		entry, _ := c.(map[string]any)
		fi, _ := entry["file_info"].(map[string]any)
		if fi == nil {
			continue
		}
		for _, key := range []string{"owner", "group"} {
			v, ok := fi[key]
			if !ok {
				continue
			}
			if !isRootOwner(v) {
				return fmt.Errorf("contents[%d].file_info.%s = %v: cooper requires owner/group 0/0 on every entry", i, key, v)
			}
		}
	}
	return nil
}

func isRootOwner(v any) bool {
	switch t := v.(type) {
	case string:
		return t == "" || t == "0" || strings.EqualFold(t, "root")
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	default:
		return false
	}
}
