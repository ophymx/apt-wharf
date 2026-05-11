package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/internal/drayman/policy"
)

// fixedNow returns a Logger pinned to a known timestamp so JSONL
// output is deterministic.
func fixedNow(buf *bytes.Buffer) *Logger {
	l := NewWriter(buf)
	l.now = func() time.Time { return time.Date(2026, 5, 11, 14, 30, 0, 0, time.UTC) }
	return l
}

func TestDecision_FullAuditContext(t *testing.T) {
	var buf bytes.Buffer
	l := fixedNow(&buf)
	d := policy.Decision{
		PackageName:  "hugo",
		Arch:         "amd64",
		Action:       policy.ActionBuild,
		Code:         policy.CodeBuildBump,
		Revision:     4,
		Reason:       "auto-bump to 4",
		PlanVersion:  "0.140.0",
		PlanHash:     "sha256:new",
		PriorVersion: "0.140.0-3",
		PriorHash:    "sha256:prior",
	}
	if err := l.Decision(d); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	var got Event
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not valid JSONL: %v\n%s", err, line)
	}
	want := Event{
		Timestamp:    "2026-05-11T14:30:00Z",
		Type:         TypeDecision,
		Package:      "hugo",
		Arch:         "amd64",
		Action:       "build",
		Code:         policy.CodeBuildBump,
		Revision:     4,
		Reason:       "auto-bump to 4",
		PlanVersion:  "0.140.0",
		PlanHash:     "sha256:new",
		PriorVersion: "0.140.0-3",
		PriorHash:    "sha256:prior",
	}
	if got != want {
		t.Errorf("decision event mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecision_RegressionEmitsRepoMax(t *testing.T) {
	var buf bytes.Buffer
	l := fixedNow(&buf)
	d := policy.Decision{
		PackageName:    "hugo",
		Arch:           "amd64",
		Action:         policy.ActionSkip,
		Code:           policy.CodeVersionRegression,
		Reason:         "regression",
		PlanVersion:    "0.140.0",
		RepoMaxVersion: "0.150.0",
	}
	if err := l.Decision(d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"repo_max_version":"0.150.0"`) {
		t.Errorf("repo_max_version not in event: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"code":"version_regression"`) {
		t.Errorf("code not in event: %s", buf.String())
	}
}

func TestImport_OkAndError(t *testing.T) {
	var buf bytes.Buffer
	l := fixedNow(&buf)
	if err := l.Import("/p/hugo_0.140.0_amd64.deb", "hugo_0.140.0_amd64.deb", nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Import("/p/broken.deb", "broken.deb", errors.New("reprepro: refused")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 events, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"result":"ok"`) || strings.Contains(lines[0], `"error"`) {
		t.Errorf("first line should be ok with no error: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"result":"error"`) || !strings.Contains(lines[1], `reprepro: refused`) {
		t.Errorf("second line should carry error: %s", lines[1])
	}
}

func TestPublish_BackendNamed(t *testing.T) {
	var buf bytes.Buffer
	l := fixedNow(&buf)
	if err := l.Publish("reprepro-local", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"backend":"reprepro-local"`) {
		t.Errorf("backend not in event: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"type":"publish"`) {
		t.Errorf("type not publish: %s", buf.String())
	}
}

func TestEvents_AreNewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	l := fixedNow(&buf)
	for range 3 {
		_ = l.Decision(policy.Decision{PackageName: "p", Arch: "amd64", Action: policy.ActionSkip, Code: policy.CodeHashInRepo})
	}
	lines := strings.Split(buf.String(), "\n")
	// Last element after a trailing \n is empty; expect 3 non-empty.
	nonEmpty := 0
	for _, l := range lines {
		if l != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 3 {
		t.Errorf("want 3 lines, got %d (raw: %q)", nonEmpty, buf.String())
	}
}
