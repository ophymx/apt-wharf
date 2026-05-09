package status

import (
	"errors"
	"testing"
	"time"

	"github.com/ophymx/apt-signpost/internal/store"
)

func TestSeed_PopulatesEnabledSourcesFromState(t *testing.T) {
	tr := NewTracker()
	checked := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	changed := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	states := map[string]*store.SourceState{
		"vendor-foo": {
			Name:        "vendor-foo",
			AssetURL:    "https://x/foo.deb",
			AssetSize:   1234,
			AssetSHA256: "abcd",
			ReleaseTag:  "v1.2.3",
			Control:     "Package: foo\nVersion: 1.2.3\nArchitecture: amd64\n",
			LastChecked: checked,
			LastChanged: changed,
		},
	}
	tr.Seed(states,
		map[string]bool{"vendor-foo": true, "vendor-bar": false},
		map[string]string{"vendor-foo": "github_release", "vendor-bar": "latest_url"})

	v := tr.Snapshot()
	foo, ok := v.Sources["vendor-foo"]
	if !ok {
		t.Fatalf("vendor-foo missing from snapshot")
	}
	if foo.Package != "foo" || foo.Version != "1.2.3" || foo.Architecture != "amd64" {
		t.Errorf("control fields not parsed: %+v", foo)
	}
	if foo.AssetSize != 1234 {
		t.Errorf("AssetSize = %d, want 1234", foo.AssetSize)
	}
	if !foo.LastCheckedAt.Equal(checked) {
		t.Errorf("LastCheckedAt = %v, want %v", foo.LastCheckedAt, checked)
	}
	if foo.DiscoveryType != "github_release" {
		t.Errorf("DiscoveryType = %q, want github_release", foo.DiscoveryType)
	}
	if !foo.Enabled {
		t.Errorf("Enabled = false, want true")
	}

	bar, ok := v.Sources["vendor-bar"]
	if !ok {
		t.Fatalf("disabled vendor-bar missing from snapshot — Seed should still register it")
	}
	if bar.Enabled {
		t.Errorf("vendor-bar Enabled = true, want false")
	}
	if bar.Version != "" {
		t.Errorf("vendor-bar has Version %q despite no state file", bar.Version)
	}
}

func TestRecordSourceOK_OverwritesError(t *testing.T) {
	tr := NewTracker()
	tr.Seed(nil, map[string]bool{"src": true}, map[string]string{"src": "latest_url"})
	tr.RecordSourceError("src", errors.New("boom"), time.Now())
	if got := tr.Snapshot().Sources["src"]; got.LastResult != "error" || got.LastError != "boom" {
		t.Fatalf("error not recorded: %+v", got)
	}

	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	tr.RecordSourceOK("src", &store.SourceState{
		Name:        "src",
		AssetURL:    "https://x",
		AssetSize:   42,
		AssetSHA256: "deadbeef",
		Control:     "Package: q\nVersion: 9\nArchitecture: amd64\n",
		LastChecked: now,
		LastChanged: now,
	}, now)
	got := tr.Snapshot().Sources["src"]
	if got.LastResult != "ok" {
		t.Errorf("LastResult = %q, want ok", got.LastResult)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty after success", got.LastError)
	}
	if got.Version != "9" {
		t.Errorf("Version = %q, want 9", got.Version)
	}
}

func TestRecordSourceUnchanged_BumpsCheckedNotChanged(t *testing.T) {
	tr := NewTracker()
	tr.Seed(map[string]*store.SourceState{
		"src": {
			Name:        "src",
			LastChecked: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			LastChanged: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		},
	}, map[string]bool{"src": true}, map[string]string{"src": "latest_url"})

	newCheck := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	tr.RecordSourceUnchanged("src", newCheck)

	got := tr.Snapshot().Sources["src"]
	if !got.LastCheckedAt.Equal(newCheck) {
		t.Errorf("LastCheckedAt = %v, want %v", got.LastCheckedAt, newCheck)
	}
	if !got.LastChangedAt.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("LastChangedAt should not move on unchanged: got %v", got.LastChangedAt)
	}
}

func TestRecordTickEnd_CountersAndDuration(t *testing.T) {
	tr := NewTracker()
	tr.RecordTickStart(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	tr.RecordTickEnd(2*time.Second, 8, 3, nil)

	v := tr.Snapshot()
	if v.Tick.OkCount != 1 || v.Tick.ErrorCount != 0 {
		t.Errorf("counters = ok=%d err=%d, want 1/0", v.Tick.OkCount, v.Tick.ErrorCount)
	}
	if v.Tick.Result != "ok" {
		t.Errorf("Result = %q, want ok", v.Tick.Result)
	}
	if v.Tick.DurationSeconds != 2 {
		t.Errorf("DurationSeconds = %v, want 2", v.Tick.DurationSeconds)
	}
	if v.Tick.SnapshotFiles != 8 || v.Tick.SnapshotRedirects != 3 {
		t.Errorf("snapshot counts = files=%d redirects=%d, want 8/3", v.Tick.SnapshotFiles, v.Tick.SnapshotRedirects)
	}

	tr.RecordTickStart(time.Now())
	tr.RecordTickEnd(time.Second, 0, 0, errors.New("sign failed"))
	v = tr.Snapshot()
	if v.Tick.OkCount != 1 || v.Tick.ErrorCount != 1 {
		t.Errorf("counters = ok=%d err=%d, want 1/1", v.Tick.OkCount, v.Tick.ErrorCount)
	}
	if v.Tick.Result != "error" || v.Tick.Error != "sign failed" {
		t.Errorf("err tick = result=%q err=%q", v.Tick.Result, v.Tick.Error)
	}
}

func TestSnapshot_DeepCopiesSources(t *testing.T) {
	tr := NewTracker()
	tr.Seed(nil, map[string]bool{"src": true}, map[string]string{"src": "latest_url"})
	tr.RecordSourceOK("src", &store.SourceState{Name: "src", AssetSize: 1}, time.Now())

	view := tr.Snapshot()
	view.Sources["src"].AssetSize = 9999

	again := tr.Snapshot()
	if again.Sources["src"].AssetSize != 1 {
		t.Errorf("Snapshot leaked tracker state: tracker AssetSize = %d", again.Sources["src"].AssetSize)
	}
}
