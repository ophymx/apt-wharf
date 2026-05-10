package build

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ophymx/apt-signpost/pkg/plan"
)

// WriteNfpmYAML decodes BuildPlan.Nfpm (json.RawMessage) into a generic
// map and writes a deterministic YAML representation at target.
// yaml.v3's default emit sorts mapping keys alphabetically, which makes
// the on-disk file byte-stable across runs of the same plan.
//
// Cooper sets `expand: true` on every contents entry as it writes
// nfpm.yaml so nfpm's per-entry env-var expansion fires for ${ASSETS}.
// This is a build-time decision (not part of BuildPlan.Nfpm or
// build_inputs_hash) — users write vanilla nfpm in doc 2 and shouldn't
// have to know about the knob.
func WriteNfpmYAML(bp plan.BuildPlan, target string) error {
	var doc any
	if err := json.Unmarshal(bp.Nfpm, &doc); err != nil {
		return fmt.Errorf("decode nfpm json: %w", err)
	}
	doc = enableContentsExpand(doc)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal nfpm yaml: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, out, 0o644)
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
