// Package audit writes a structured, append-only event log for
// drayman runs. cooper-design.md §"Audit & safety" makes this an
// orchestrator concern: log every auto-bump (prior hash → new hash,
// prior revision → new revision), every regression skip, and every
// import/publish event so operators can review what drayman did
// without re-deriving it from peek output.
//
// The log is JSON-lines (one event per line, append-mode) so it
// composes with jq, logrotate, and tail -F. Events are
// self-contained — no schema version yet because drayman's audit
// surface is small enough that a future bump can just add a new
// event type rather than reshape the existing ones.
//
// Concurrency: the Logger holds a mutex around writes. POSIX O_APPEND
// is per-write atomic up to PIPE_BUF (~4KiB), which our lines fit
// inside, but the mutex makes Log calls safe across goroutines in a
// single drayman process. Multiple drayman processes sharing one log
// file rely on POSIX append semantics.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ophymx/apt-signpost/internal/drayman/policy"
)

// Event is one audit record. Fields are populated based on Type;
// see field-level comments. JSON tags use snake_case for jq-friendliness.
type Event struct {
	Timestamp string `json:"ts"`   // RFC3339 UTC
	Type      string `json:"type"` // see Type* constants

	// Decision fields (Type == TypeDecision).
	Package        string `json:"package,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Action         string `json:"action,omitempty"`
	Code           string `json:"code,omitempty"`
	Revision       int    `json:"revision,omitempty"`
	Reason         string `json:"reason,omitempty"`
	PlanVersion    string `json:"plan_version,omitempty"`
	PlanHash       string `json:"plan_hash,omitempty"`
	RepoMaxVersion string `json:"repo_max_version,omitempty"`
	PriorVersion   string `json:"prior_version,omitempty"`
	PriorHash      string `json:"prior_hash,omitempty"`

	// Import fields (Type == TypeImport).
	DebPath     string `json:"deb_path,omitempty"`
	DebBasename string `json:"deb_basename,omitempty"`

	// Publish fields (Type == TypePublish).
	Backend string `json:"backend,omitempty"`

	// Generic fields.
	Result string `json:"result,omitempty"` // "ok" | "error"
	Error  string `json:"error,omitempty"`
}

// Type values for Event.Type.
const (
	TypeDecision = "decision"
	TypeImport   = "import"
	TypePublish  = "publish"
)

// Result values for Event.Result.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Logger writes audit events as JSONL to an io.Writer. Construct
// with Open (file) or NewWriter (custom destination, e.g. tests).
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer // non-nil only when Open created the underlying file
	now    func() time.Time
}

// Open opens (or creates) path for append-only audit logging. The
// caller must Close the returned Logger; failing to do so loses
// any buffered writes (none today, but the contract reserves the
// option).
func Open(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	l := NewWriter(f)
	l.closer = f
	return l, nil
}

// NewWriter returns a Logger that writes events to w. Tests use this
// with bytes.Buffer; production uses Open.
func NewWriter(w io.Writer) *Logger {
	return &Logger{w: w, now: time.Now}
}

// Close releases the underlying file handle when Open was used.
// Safe to call on a Writer-constructed Logger (no-op).
func (l *Logger) Close() error {
	if l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// Decision writes a decision event derived from policy.Decision.
func (l *Logger) Decision(d policy.Decision) error {
	return l.write(Event{
		Type:           TypeDecision,
		Package:        d.PackageName,
		Arch:           d.Arch,
		Action:         string(d.Action),
		Code:           d.Code,
		Revision:       d.Revision,
		Reason:         d.Reason,
		PlanVersion:    d.PlanVersion,
		PlanHash:       d.PlanHash,
		RepoMaxVersion: d.RepoMaxVersion,
		PriorVersion:   d.PriorVersion,
		PriorHash:      d.PriorHash,
	})
}

// Import writes an import event. err is nil for success.
func (l *Logger) Import(debPath, basename string, err error) error {
	e := Event{
		Type:        TypeImport,
		DebPath:     debPath,
		DebBasename: basename,
		Result:      ResultOK,
	}
	if err != nil {
		e.Result = ResultError
		e.Error = err.Error()
	}
	return l.write(e)
}

// Publish writes a publish event. err is nil for success.
func (l *Logger) Publish(backend string, err error) error {
	e := Event{
		Type:    TypePublish,
		Backend: backend,
		Result:  ResultOK,
	}
	if err != nil {
		e.Result = ResultError
		e.Error = err.Error()
	}
	return l.write(e)
}

func (l *Logger) write(e Event) error {
	e.Timestamp = l.now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(b); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}
