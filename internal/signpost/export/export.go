// Package export materializes a refresh.Snapshot as a static site tree
// plus a host-specific redirect manifest, so the repository signpost
// would serve dynamically can instead be deployed to a static host.
//
// The snapshot is already the whole served state (files map + redirects
// map; see internal/signpost/refresh/snapshot.go), so an export is a
// faithful transcription: every file key becomes a file under the output
// root, and every redirect key becomes one entry in whatever redirect
// format the chosen Target understands.
//
// Write discipline:
//
//   - Every file is written via tmp + rename, so a web server pointed at
//     the output root never serves a partial file.
//   - Files are written leaves-first (pool + by-hash, then canonical
//     Packages, then Release/InRelease last) so a client that reads the
//     new InRelease finds the by-hash entries it references already present.
//   - A marker file (MarkerName) records every path the previous export
//     wrote. Paths in the previous marker but not in the current export
//     are pruned; nothing else in the output root is ever removed. An
//     existing non-empty output root without a marker is refused.
package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
)

// MarkerName is the file at the output root that records what the last
// export wrote. It is the prune list and the "this directory is ours"
// guard. Hosts serve it as a tiny JSON file; it holds nothing sensitive.
const MarkerName = ".signpost-export.json"

// markerVersion bumps on incompatible marker shape changes.
const markerVersion = 1

type marker struct {
	Version int       `json:"version"`
	BuiltAt time.Time `json:"built_at"`
	// Paths are output-root-relative, slash-separated, no leading slash.
	// Includes target-emitted files but not the marker itself.
	Paths []string `json:"paths"`
}

// Options controls one export.
type Options struct {
	// OutDir is the output root. Created if missing.
	OutDir string
	// Targets emit host-specific redirect/header files. At least one.
	Targets []Target
}

// Result reports what Write did.
type Result struct {
	Written int // files written this run (site tree + target files)
	Pruned  int // stale files removed from a previous export
}

// Write exports snap into opts.OutDir. See the package comment for the
// write discipline.
func Write(snap *refresh.Snapshot, opts Options) (*Result, error) {
	if snap == nil {
		return nil, errors.New("export: nil snapshot")
	}
	if opts.OutDir == "" {
		return nil, errors.New("export: OutDir is required")
	}
	if len(opts.Targets) == 0 {
		return nil, errors.New("export: at least one target is required")
	}

	m, err := BuildManifest(snap)
	if err != nil {
		return nil, err
	}

	// Collect everything to write: site tree first, then target files.
	out := map[string][]byte{}
	for p, f := range snap.Files {
		rel, err := relPath(p)
		if err != nil {
			return nil, err
		}
		out[rel] = f.Data
	}
	for _, t := range opts.Targets {
		files, err := t.Emit(m)
		if err != nil {
			return nil, fmt.Errorf("export: target %s: %w", t.Name(), err)
		}
		for p, data := range files {
			rel, err := relPath("/" + strings.TrimPrefix(p, "/"))
			if err != nil {
				return nil, fmt.Errorf("export: target %s: %w", t.Name(), err)
			}
			if _, clash := out[rel]; clash {
				return nil, fmt.Errorf("export: target %s: %s collides with an existing output file", t.Name(), rel)
			}
			out[rel] = data
		}
	}

	prev, err := readMarker(opts.OutDir)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("export: mkdir %s: %w", opts.OutDir, err)
	}

	res := &Result{}
	for _, rel := range writeOrder(out) {
		if err := writeFileAtomic(filepath.Join(opts.OutDir, filepath.FromSlash(rel)), out[rel]); err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		res.Written++
	}

	// Prune: anything the previous export wrote that this one did not.
	if prev != nil {
		for _, rel := range prev.Paths {
			if _, keep := out[rel]; keep {
				continue
			}
			abs := filepath.Join(opts.OutDir, filepath.FromSlash(rel))
			if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("export: prune %s: %w", rel, err)
			}
			res.Pruned++
			removeEmptyParents(opts.OutDir, filepath.Dir(abs))
		}
	}

	paths := make([]string, 0, len(out))
	for rel := range out {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	mk := marker{Version: markerVersion, BuiltAt: snap.BuiltAt.UTC(), Paths: paths}
	body, err := json.MarshalIndent(mk, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export: marshal marker: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(opts.OutDir, MarkerName), append(body, '\n')); err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	return res, nil
}

// readMarker returns the previous export's marker, or nil when OutDir
// does not exist or is empty. A non-empty OutDir without a marker is
// refused: we never prune or overwrite a directory we did not write.
func readMarker(outDir string) (*marker, error) {
	st, err := os.Stat(outDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("export: stat %s: %w", outDir, err)
	case !st.IsDir():
		return nil, fmt.Errorf("export: %s exists and is not a directory", outDir)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, MarkerName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("export: read marker: %w", err)
		}
		entries, derr := os.ReadDir(outDir)
		if derr != nil {
			return nil, fmt.Errorf("export: read %s: %w", outDir, derr)
		}
		if len(entries) > 0 {
			return nil, fmt.Errorf("export: refusing to write into non-empty %s: no %s marker from a previous export", outDir, MarkerName)
		}
		return nil, nil
	}
	var mk marker
	if err := json.Unmarshal(raw, &mk); err != nil {
		return nil, fmt.Errorf("export: parse %s: %w", MarkerName, err)
	}
	if mk.Version != markerVersion {
		return nil, fmt.Errorf("export: %s has version %d; this build understands %d", MarkerName, mk.Version, markerVersion)
	}
	for _, p := range mk.Paths {
		if _, err := relPath("/" + p); err != nil {
			return nil, fmt.Errorf("export: %s lists unsafe path %q", MarkerName, p)
		}
	}
	return &mk, nil
}

// relPath converts a snapshot path ("/dists/stable/InRelease") into a
// clean output-root-relative path, rejecting anything that could escape
// the root. Snapshot keys are generated internally, so this is a
// defensive check rather than an expected failure.
func relPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q does not start with /", p)
	}
	cleaned := path.Clean(p)
	if cleaned != p || cleaned == "/" {
		return "", fmt.Errorf("path %q is not canonical", p)
	}
	for seg := range strings.SplitSeq(cleaned[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("path %q has an unsafe segment", p)
		}
	}
	rel := cleaned[1:]
	if rel == MarkerName {
		return "", fmt.Errorf("path %q is reserved for the export marker", p)
	}
	return rel, nil
}

// writeOrder sorts output paths leaves-first: everything outside /dists
// (pool .debs, pubkey, target files), then by-hash, then canonical
// Packages, then Release.gpg, Release, and finally InRelease. Within a
// tier the order is lexical for determinism.
func writeOrder(out map[string][]byte) []string {
	tier := func(rel string) int {
		switch {
		case !strings.HasPrefix(rel, "dists/"):
			return 0
		case strings.Contains(rel, "/by-hash/"):
			return 1
		case path.Base(rel) == "InRelease":
			return 5
		case path.Base(rel) == "Release":
			return 4
		case path.Base(rel) == "Release.gpg":
			return 3
		default:
			return 2
		}
	}
	paths := make([]string, 0, len(out))
	for rel := range out {
		paths = append(paths, rel)
	}
	sort.Slice(paths, func(i, j int) bool {
		ti, tj := tier(paths[i]), tier(paths[j])
		if ti != tj {
			return ti < tj
		}
		return paths[i] < paths[j]
	})
	return paths
}

func writeFileAtomic(abs string, data []byte) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-export-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return fmt.Errorf("rename to %s: %w", abs, err)
	}
	ok = true
	return nil
}

// removeEmptyParents walks up from dir toward root, removing empty
// directories. Best effort: any error stops the walk silently.
func removeEmptyParents(root, dir string) {
	root = filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
