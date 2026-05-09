// Package store handles per-source and bootstrap state persistence.
//
// Layout:
//
//	state/
//	  sources/<name>.json
//	  bootstrap.json
//	  bootstrap/<version>.deb
//
// Writes are tmp-file + rename to make every update atomic on POSIX. There
// is one writer (the refresh goroutine), so no locking is needed beyond
// what the refresh-tick mutex already provides.
package store

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SourceState is the per-source on-disk record. Schema mirrors design-mvp.md.
type SourceState struct {
	Name        string    `json:"id"` // "id" preserved for human readability
	ReleaseID   int64     `json:"release_id"`
	ReleaseTag  string    `json:"release_tag"`
	AssetURL    string    `json:"asset_url"`
	AssetSize   int64     `json:"asset_size"`
	AssetSHA256 string    `json:"asset_sha256"`
	APIEtag     string    `json:"api_etag,omitempty"`
	Control     string    `json:"control"`
	LastChecked time.Time `json:"last_checked"`
	LastChanged time.Time `json:"last_changed"`
}

// BootstrapState records the cached bootstrap .deb's identity. The .deb bytes
// themselves live at bootstrap/<version>.deb.
type BootstrapState struct {
	Version   string `json:"version"`
	InputHash string `json:"input_hash"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Store is the on-disk root.
type Store struct{ root string }

func New(stateDir string) *Store { return &Store{root: stateDir} }

func (s *Store) Root() string { return s.root }

func (s *Store) sourceDir() string { return filepath.Join(s.root, "sources") }
func (s *Store) bootstrapDir() string { return filepath.Join(s.root, "bootstrap") }

// EnsureDirs creates state subdirectories. Idempotent.
func (s *Store) EnsureDirs() error {
	for _, d := range []string{s.root, s.sourceDir(), s.bootstrapDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

// LoadSources reads every state/sources/*.json. Missing directory yields an
// empty map (cold start). Malformed file yields a hard error rather than
// silently dropping a source — strict-error policy.
func (s *Store) LoadSources() (map[string]*SourceState, error) {
	out := make(map[string]*SourceState)
	entries, err := os.ReadDir(s.sourceDir())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read source dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		path := filepath.Join(s.sourceDir(), e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var st SourceState
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		// Filename is canonical; mismatch with "id" field flags hand-edits.
		if st.Name != "" && st.Name != name {
			return nil, fmt.Errorf("%s: id field %q does not match filename %q",
				path, st.Name, name)
		}
		st.Name = name
		out[name] = &st
	}
	return out, nil
}

func (s *Store) sourcePath(name string) string {
	return filepath.Join(s.sourceDir(), name+".json")
}

// WriteSource serializes state and renames into place atomically.
func (s *Store) WriteSource(state *SourceState) error {
	if state.Name == "" {
		return fmt.Errorf("WriteSource: empty name")
	}
	return writeJSONAtomic(s.sourcePath(state.Name), state)
}

// LoadBootstrap returns nil, nil when no bootstrap.json exists yet.
func (s *Store) LoadBootstrap() (*BootstrapState, error) {
	path := filepath.Join(s.root, "bootstrap.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var st BootstrapState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &st, nil
}

func (s *Store) WriteBootstrap(state *BootstrapState) error {
	return writeJSONAtomic(filepath.Join(s.root, "bootstrap.json"), state)
}

// LoadBootstrapDeb returns the cached .deb bytes for the recorded version.
func (s *Store) LoadBootstrapDeb(version string) ([]byte, error) {
	path := filepath.Join(s.bootstrapDir(), version+".deb")
	return os.ReadFile(path)
}

func (s *Store) WriteBootstrapDeb(version string, body []byte) error {
	path := filepath.Join(s.bootstrapDir(), version+".deb")
	return writeBytesAtomic(path, body, 0o644)
}

func writeJSONAtomic(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body = append(body, '\n')
	return writeBytesAtomic(path, body, 0o644)
}

func writeBytesAtomic(path string, body []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	renamed = true
	return nil
}
