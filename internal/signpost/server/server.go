// Package server is the HTTP front end. One handler reads the atomic
// snapshot once at the top of each request, then serves bytes / 302 / 404.
// The /status and /metrics admin endpoints are routed off the snapshot
// path; everything else falls through to the apt-repo serving logic.
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
	"github.com/ophymx/apt-wharf/internal/signpost/status"
)

// Handler returns an http.Handler backed by the snapshot held in h, with
// observability endpoints sourced from the status tracker (which may be
// nil — handlers degrade to "no data" rather than panicking).
//
// Every request produces one access-log line at INFO with method, path,
// status, bytes, kind (file/redirect/404/unavailable/status/metrics),
// duration, and remote address.
//
// The admin endpoints are NOT authenticated — operators are expected to
// ACL them at the front proxy if the listener is public.
func Handler(h *refresh.Holder, tracker *status.Tracker, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	statusH := statusHandler(h, tracker)
	metricsH := metricsHandler(h, tracker)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recordingWriter{ResponseWriter: w}

		var kind string
		switch r.URL.Path {
		case "/status":
			statusH.ServeHTTP(rw, r)
			kind = "status"
		case "/metrics":
			metricsH.ServeHTTP(rw, r)
			kind = "metrics"
		default:
			kind = serveSnapshot(rw, r, h)
		}

		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusOrDefault(),
			"bytes", rw.bytes,
			"kind", kind,
			"dur", time.Since(start).String(),
			"remote", r.RemoteAddr,
			"ua", r.Header.Get("User-Agent"),
		)
	})
}

// serveSnapshot is the routing core; returns a short string tag describing
// the outcome (used by the access log).
func serveSnapshot(w http.ResponseWriter, r *http.Request, h *refresh.Holder) string {
	snap := h.Load()
	if snap == nil {
		http.Error(w, "snapshot not yet built", http.StatusServiceUnavailable)
		return "unavailable"
	}
	f, rd, isFile, isRedirect := snap.Lookup(r.URL.Path)
	switch {
	case isFile:
		if f.ContentType != "" {
			w.Header().Set("Content-Type", f.ContentType)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(f.Data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(f.Data)
		}
		return "file"
	case isRedirect:
		http.Redirect(w, r, rd.URL, http.StatusFound)
		return "redirect"
	default:
		http.NotFound(w, r)
		return "404"
	}
}

// recordingWriter captures the status code + bytes written so the access
// log line can include them after the handler returns.
type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recordingWriter) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingWriter) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recordingWriter) statusOrDefault() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
