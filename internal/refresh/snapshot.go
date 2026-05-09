// Package refresh orchestrates per-tick discovery, fetch, index, sign, and
// snapshot-publish. The published snapshot is a single atomic.Pointer.Store
// — readers (HTTP handlers) load it once at the top of each request.
package refresh

import (
	"sync/atomic"
	"time"
)

// FileEntry is one served file. ExpiresAt is consulted only at composition
// time; once a file is in a published snapshot, it serves until the next
// snapshot replaces it.
type FileEntry struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time // zero = never expires (gets overwritten in next snapshot anyway)
}

// Redirect is a synthetic upstream-redirected pool path.
type Redirect struct {
	URL       string
	ExpiresAt time.Time
}

// Snapshot is the published served state. Readers swap in via atomic.Pointer.
type Snapshot struct {
	Files     map[string]FileEntry
	Redirects map[string]Redirect
	BuiltAt   time.Time
}

// Lookup is the read-side helper. Returns:
//   - file bytes + true when path is a static file
//   - redirect URL + true when path is a 302
//   - all-zero + false on miss
func (s *Snapshot) Lookup(path string) (FileEntry, Redirect, bool, bool) {
	if f, ok := s.Files[path]; ok {
		return f, Redirect{}, true, false
	}
	if r, ok := s.Redirects[path]; ok {
		return FileEntry{}, r, false, true
	}
	return FileEntry{}, Redirect{}, false, false
}

// Holder is a thin wrapper around atomic.Pointer[Snapshot] so callers don't
// need to think about pointer lifetimes.
type Holder struct {
	p atomic.Pointer[Snapshot]
}

func (h *Holder) Load() *Snapshot { return h.p.Load() }
func (h *Holder) Store(s *Snapshot) { h.p.Store(s) }
