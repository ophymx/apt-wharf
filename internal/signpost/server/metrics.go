package server

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
	"github.com/ophymx/apt-wharf/internal/signpost/status"
)

// metricsHandler exposes Prometheus text-format metrics derived from the
// status tracker plus the served snapshot. The exposition format is
// hand-written to avoid pulling in client_golang for this handful of
// gauges + counters.
//
// All emitted metric names are prefixed signpost_; all label values are
// bounded-cardinality (source names from config, version strings, the
// literal "ok"/"error"), so cardinality is bounded by the source count.
func metricsHandler(holder *refresh.Holder, tracker *status.Tracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		view := status.SnapshotView{Sources: map[string]*status.SourceStatus{}}
		if tracker != nil {
			view = tracker.Snapshot()
		}

		var files, redirects int
		if snap := holder.Load(); snap != nil {
			files = len(snap.Files)
			redirects = len(snap.Redirects)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		writeMetrics(w, view, files, redirects)
	})
}

func writeMetrics(w io.Writer, v status.SnapshotView, files, redirects int) {
	// Refresh tick.
	help(w, "signpost_refresh_tick_total",
		"Number of refresh ticks completed, by result.")
	typ(w, "signpost_refresh_tick_total", "counter")
	fmt.Fprintf(w, "signpost_refresh_tick_total{result=\"ok\"} %d\n", v.Tick.OkCount)
	fmt.Fprintf(w, "signpost_refresh_tick_total{result=\"error\"} %d\n", v.Tick.ErrorCount)

	help(w, "signpost_refresh_tick_last_duration_seconds",
		"Duration of the most recent refresh tick.")
	typ(w, "signpost_refresh_tick_last_duration_seconds", "gauge")
	fmt.Fprintf(w, "signpost_refresh_tick_last_duration_seconds %g\n", v.Tick.DurationSeconds)

	help(w, "signpost_refresh_tick_last_timestamp_seconds",
		"Unix timestamp of the most recent refresh tick start (0 if never run).")
	typ(w, "signpost_refresh_tick_last_timestamp_seconds", "gauge")
	fmt.Fprintf(w, "signpost_refresh_tick_last_timestamp_seconds %d\n",
		unixOrZero(v.Tick.StartedAt))

	// Sources — sorted by name for stable output.
	names := make([]string, 0, len(v.Sources))
	for n := range v.Sources {
		names = append(names, n)
	}
	sort.Strings(names)

	help(w, "signpost_source_last_checked_timestamp_seconds",
		"Unix timestamp of the most recent check attempt for the source.")
	typ(w, "signpost_source_last_checked_timestamp_seconds", "gauge")
	for _, n := range names {
		s := v.Sources[n]
		fmt.Fprintf(w, "signpost_source_last_checked_timestamp_seconds{source=%s} %d\n",
			labelValue(n), unixOrZero(s.LastCheckedAt))
	}

	help(w, "signpost_source_last_changed_timestamp_seconds",
		"Unix timestamp of the most recent detected change for the source.")
	typ(w, "signpost_source_last_changed_timestamp_seconds", "gauge")
	for _, n := range names {
		s := v.Sources[n]
		fmt.Fprintf(w, "signpost_source_last_changed_timestamp_seconds{source=%s} %d\n",
			labelValue(n), unixOrZero(s.LastChangedAt))
	}

	help(w, "signpost_source_status",
		"Most recent source attempt outcome: 1=ok, 0=error, NaN=never attempted.")
	typ(w, "signpost_source_status", "gauge")
	for _, n := range names {
		s := v.Sources[n]
		switch s.LastResult {
		case "ok":
			fmt.Fprintf(w, "signpost_source_status{source=%s} 1\n", labelValue(n))
		case "error":
			fmt.Fprintf(w, "signpost_source_status{source=%s} 0\n", labelValue(n))
		default:
			fmt.Fprintf(w, "signpost_source_status{source=%s} NaN\n", labelValue(n))
		}
	}

	help(w, "signpost_source_asset_size_bytes",
		"Size of the most recently known asset for the source.")
	typ(w, "signpost_source_asset_size_bytes", "gauge")
	for _, n := range names {
		s := v.Sources[n]
		fmt.Fprintf(w, "signpost_source_asset_size_bytes{source=%s} %d\n",
			labelValue(n), s.AssetSize)
	}

	// Bootstrap.
	help(w, "signpost_bootstrap_version_info",
		"Active bootstrap package version (label-only; value is always 1).")
	typ(w, "signpost_bootstrap_version_info", "gauge")
	if v.Bootstrap.Version != "" {
		fmt.Fprintf(w, "signpost_bootstrap_version_info{version=%s} 1\n",
			labelValue(v.Bootstrap.Version))
	}

	help(w, "signpost_bootstrap_size_bytes",
		"Size in bytes of the active bootstrap .deb.")
	typ(w, "signpost_bootstrap_size_bytes", "gauge")
	fmt.Fprintf(w, "signpost_bootstrap_size_bytes %d\n", v.Bootstrap.Size)

	help(w, "signpost_bootstrap_last_built_timestamp_seconds",
		"Unix timestamp of the most recent bootstrap rebuild in this process (0 if not yet rebuilt).")
	typ(w, "signpost_bootstrap_last_built_timestamp_seconds", "gauge")
	fmt.Fprintf(w, "signpost_bootstrap_last_built_timestamp_seconds %d\n",
		unixOrZero(v.Bootstrap.LastBuiltAt))

	// Snapshot.
	help(w, "signpost_snapshot_files",
		"Number of static files in the currently published snapshot.")
	typ(w, "signpost_snapshot_files", "gauge")
	fmt.Fprintf(w, "signpost_snapshot_files %d\n", files)

	help(w, "signpost_snapshot_redirects",
		"Number of pool-path redirects in the currently published snapshot.")
	typ(w, "signpost_snapshot_redirects", "gauge")
	fmt.Fprintf(w, "signpost_snapshot_redirects %d\n", redirects)
}

func help(w io.Writer, name, text string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, text)
}

func typ(w io.Writer, name, kind string) {
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
}

// labelValue produces a quoted Prometheus label value with the three
// required escape sequences (\\, \", \n). Source names from config and
// version strings won't contain any of these in practice, but cheap
// insurance against future label additions that might.
func labelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// unixOrZero returns t.Unix() unless t is the zero time, in which case it
// returns 0. The default time.Time.Unix() for zero is a large negative
// number that confuses Prometheus alerting (`time() - X > threshold`).
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
