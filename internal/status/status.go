// Package status holds the in-memory observability snapshot that backs the
// /status and /metrics HTTP endpoints. It records what the refresher last
// did for each source, the last bootstrap rebuild, and the most recent
// refresh tick — none of which fit into the served apt-repo Snapshot.
//
// The Tracker is updated by the refresher (one writer per field set, behind
// a single mutex) and read by HTTP handlers. Reads are cheap and produce a
// frozen copy via Snapshot() so callers don't see torn state.
package status

import (
	"sync"
	"time"

	"github.com/ophymx/apt-signpost/internal/index"
	"github.com/ophymx/apt-signpost/internal/store"
)

// SourceStatus captures the last-known state of one source, both content
// and operational outcome. Empty fields mean "we don't know yet" — e.g. on
// cold start before the source has ever been processed.
type SourceStatus struct {
	Name          string    `json:"name"`
	DiscoveryType string    `json:"discovery_type"`
	Enabled       bool      `json:"enabled"`
	LastCheckedAt time.Time `json:"last_checked_at,omitzero"`
	LastChangedAt time.Time `json:"last_changed_at,omitzero"`
	LastResult    string    `json:"last_result,omitempty"` // "ok" | "error" | ""
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`

	// Content fields, mirrored from the most recent successful state file.
	Package      string `json:"package,omitempty"`
	Version      string `json:"version,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	ReleaseTag   string `json:"release_tag,omitempty"`
	AssetURL     string `json:"asset_url,omitempty"`
	AssetSize    int64  `json:"asset_size,omitempty"`
	AssetSHA256  string `json:"asset_sha256,omitempty"`
}

// BootstrapStatus mirrors the latest BootstrapState plus a timestamp for
// when this process last performed a rebuild.
type BootstrapStatus struct {
	Version     string    `json:"version,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	Size        int64     `json:"size,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	InputHash   string    `json:"input_hash,omitempty"`
	LastBuiltAt time.Time `json:"last_built_at,omitzero"`
}

// TickStatus describes the most recent refresh tick plus running totals.
type TickStatus struct {
	StartedAt           time.Time     `json:"started_at,omitzero"`
	Duration            time.Duration `json:"-"`
	DurationSeconds     float64       `json:"duration_seconds,omitempty"`
	Result              string        `json:"result,omitempty"` // "ok" | "error"
	Error               string        `json:"error,omitempty"`
	OkCount             uint64        `json:"ok_count"`
	ErrorCount          uint64        `json:"error_count"`
	SnapshotFiles       int           `json:"snapshot_files,omitempty"`
	SnapshotRedirects   int           `json:"snapshot_redirects,omitempty"`
}

// SnapshotView is a frozen copy of the tracker's contents suitable for
// JSON marshalling or metrics rendering. Maps are independent of the
// tracker's live state, so callers can iterate them without holding any
// lock.
type SnapshotView struct {
	Tick      TickStatus               `json:"tick"`
	Bootstrap BootstrapStatus          `json:"bootstrap"`
	Sources   map[string]*SourceStatus `json:"sources"`
}

// Tracker is the central observability store. Safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	sources   map[string]*SourceStatus
	bootstrap BootstrapStatus
	tick      TickStatus
}

// NewTracker creates an empty Tracker. Use Seed to prepopulate from on-disk
// state at startup; otherwise the Tracker starts with no source records and
// fills in as the refresher processes each source.
func NewTracker() *Tracker {
	return &Tracker{sources: map[string]*SourceStatus{}}
}

// Seed initializes (or replaces) the tracker's per-source records from the
// SourceState map loaded at startup, plus a per-source enabled / discovery
// type lookup for fields that don't live in the state file. Anything not
// in 'enabled' is omitted from the tracker — disabled sources are not
// observable until re-enabled and processed.
//
// enabled and discoveryTypes are keyed by source name.
func (t *Tracker) Seed(states map[string]*store.SourceState, enabled map[string]bool, discoveryTypes map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sources = map[string]*SourceStatus{}
	for name, isEnabled := range enabled {
		s := &SourceStatus{
			Name:          name,
			DiscoveryType: discoveryTypes[name],
			Enabled:       isEnabled,
		}
		if st, ok := states[name]; ok {
			fillFromState(s, st)
		}
		t.sources[name] = s
	}
}

// SeedBootstrap initializes the bootstrap record from the persisted state
// without claiming it was rebuilt this process — LastBuiltAt stays zero
// until RecordBootstrapBuild fires.
func (t *Tracker) SeedBootstrap(bs *store.BootstrapState) {
	if bs == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bootstrap = BootstrapStatus{
		Version:   bs.Version,
		Filename:  bs.Filename,
		Size:      bs.Size,
		SHA256:    bs.SHA256,
		InputHash: bs.InputHash,
	}
}

// RecordSourceOK records a successful processOne outcome for source name.
// state is the SourceState that was just persisted; used to mirror content
// fields into the SourceStatus.
func (t *Tracker) RecordSourceOK(name string, state *store.SourceState, when time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensure(name)
	s.LastCheckedAt = when
	s.LastResult = "ok"
	s.LastError = ""
	s.LastErrorAt = time.Time{}
	if state != nil {
		fillFromState(s, state)
	}
}

// RecordSourceUnchanged records that processOne ran and confirmed the source
// hasn't changed since the last tick — bumps last_checked but leaves
// last_changed and content fields alone.
func (t *Tracker) RecordSourceUnchanged(name string, when time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensure(name)
	s.LastCheckedAt = when
	s.LastResult = "ok"
	s.LastError = ""
	s.LastErrorAt = time.Time{}
}

// RecordSourceError records a failed processOne outcome. Content fields
// from the last successful run are preserved so /status can show "last
// known good" version alongside the error.
func (t *Tracker) RecordSourceError(name string, err error, when time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensure(name)
	s.LastCheckedAt = when
	s.LastResult = "error"
	if err != nil {
		s.LastError = err.Error()
	}
	s.LastErrorAt = when
}

// RecordBootstrapBuild records the latest bootstrap rebuild. Called only
// when nfpm actually re-ran (cache hits don't bump LastBuiltAt).
func (t *Tracker) RecordBootstrapBuild(bs *store.BootstrapState, when time.Time) {
	if bs == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bootstrap = BootstrapStatus{
		Version:     bs.Version,
		Filename:    bs.Filename,
		Size:        bs.Size,
		SHA256:      bs.SHA256,
		InputHash:   bs.InputHash,
		LastBuiltAt: when,
	}
}

// RecordTickStart marks the start of a refresh tick.
func (t *Tracker) RecordTickStart(when time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tick.StartedAt = when
}

// RecordTickEnd records the outcome of a refresh tick. files/redirects
// reflect the freshly published snapshot when err == nil.
func (t *Tracker) RecordTickEnd(duration time.Duration, files, redirects int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tick.Duration = duration
	t.tick.DurationSeconds = duration.Seconds()
	if err == nil {
		t.tick.Result = "ok"
		t.tick.Error = ""
		t.tick.OkCount++
		t.tick.SnapshotFiles = files
		t.tick.SnapshotRedirects = redirects
	} else {
		t.tick.Result = "error"
		t.tick.Error = err.Error()
		t.tick.ErrorCount++
	}
}

// Snapshot returns a deep-copy of the tracker's current state. Safe to
// marshal or iterate without holding any lock.
func (t *Tracker) Snapshot() SnapshotView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := SnapshotView{
		Tick:      t.tick,
		Bootstrap: t.bootstrap,
		Sources:   make(map[string]*SourceStatus, len(t.sources)),
	}
	for k, v := range t.sources {
		copyVal := *v
		out.Sources[k] = &copyVal
	}
	return out
}

// ensure returns the SourceStatus for name, creating one if missing. Caller
// must hold t.mu.
func (t *Tracker) ensure(name string) *SourceStatus {
	if s, ok := t.sources[name]; ok {
		return s
	}
	s := &SourceStatus{Name: name, Enabled: true}
	t.sources[name] = s
	return s
}

// fillFromState mirrors the content-bearing fields of a SourceState into a
// SourceStatus. Best-effort control parse: failure leaves Package/Version/
// Architecture empty rather than aborting (we'd rather show partial data
// than nothing).
func fillFromState(s *SourceStatus, st *store.SourceState) {
	s.LastCheckedAt = st.LastChecked
	s.LastChangedAt = st.LastChanged
	s.AssetURL = st.AssetURL
	s.AssetSize = st.AssetSize
	s.AssetSHA256 = st.AssetSHA256
	s.ReleaseTag = st.ReleaseTag
	if st.Control == "" {
		return
	}
	if fs, err := index.ExtractFields([]byte(st.Control)); err == nil {
		s.Package = fs.Package
		s.Version = fs.Version
		s.Architecture = fs.Architecture
	}
}
