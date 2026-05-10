package server

import (
	"encoding/json"
	"net/http"

	"github.com/ophymx/apt-signpost/internal/refresh"
	"github.com/ophymx/apt-signpost/internal/status"
)

// statusHandler returns a JSON view of the tracker plus a few derived
// fields from the served snapshot. Stable shape suitable for tooling.
//
// Operators are expected to ACL this endpoint via the front proxy if the
// listener is public; signpost itself does not authenticate /status.
func statusHandler(holder *refresh.Holder, tracker *status.Tracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		view := status.SnapshotView{
			Sources: map[string]*status.SourceStatus{},
		}
		if tracker != nil {
			view = tracker.Snapshot()
		}

		// Annotate with snapshot-derived fields the tracker doesn't own.
		snapInfo := snapshotInfo{}
		if snap := holder.Load(); snap != nil {
			snapInfo.Files = len(snap.Files)
			snapInfo.Redirects = len(snap.Redirects)
			snapInfo.BuiltAt = snap.BuiltAt.Format("2006-01-02T15:04:05Z07:00")
		}

		out := statusResponse{
			Snapshot:  snapInfo,
			Tick:      view.Tick,
			Bootstrap: view.Bootstrap,
			Sources:   view.Sources,
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})
}

type statusResponse struct {
	Snapshot  snapshotInfo                    `json:"snapshot"`
	Tick      status.TickStatus               `json:"tick"`
	Bootstrap status.BootstrapStatus          `json:"bootstrap"`
	Sources   map[string]*status.SourceStatus `json:"sources"`
}

type snapshotInfo struct {
	Files     int    `json:"files"`
	Redirects int    `json:"redirects"`
	BuiltAt   string `json:"built_at,omitempty"`
}
