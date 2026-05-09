package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStore_SourceRoundtrip(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	in := &SourceState{
		Name:        "vendor-bar-amd64",
		ReleaseID:   123,
		ReleaseTag:  "v1.2.3",
		AssetURL:    "https://example.com/bar.deb",
		AssetSize:   456,
		AssetSHA256: "deadbeef",
		APIEtag:     `W/"abc"`,
		Control:     "Package: bar\nVersion: 1.2.3\nArchitecture: amd64\n",
		LastChecked: time.Now().UTC().Truncate(time.Second),
		LastChanged: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.WriteSource(in); err != nil {
		t.Fatalf("WriteSource: %v", err)
	}

	loaded, err := s.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	got := loaded["vendor-bar-amd64"]
	if got == nil {
		t.Fatalf("source not loaded")
	}
	if got.AssetSHA256 != in.AssetSHA256 || got.ReleaseID != in.ReleaseID {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, in)
	}
}

func TestStore_LoadSourcesEmptyDir(t *testing.T) {
	s := New(t.TempDir())
	loaded, err := s.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources on missing dir: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(loaded))
	}
}

func TestStore_BootstrapRoundtrip(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	in := &BootstrapState{
		Version:   "2026.05.09.1",
		InputHash: "sha256:abc",
		Filename:  "acme-archive-keyring_2026.05.09.1_all.deb",
		Size:      1024,
		SHA256:    "feedface",
	}
	if err := s.WriteBootstrap(in); err != nil {
		t.Fatalf("WriteBootstrap: %v", err)
	}
	got, err := s.LoadBootstrap()
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if got == nil || got.Version != in.Version || got.SHA256 != in.SHA256 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	body := []byte("fake deb bytes")
	if err := s.WriteBootstrapDeb(in.Version, body); err != nil {
		t.Fatalf("WriteBootstrapDeb: %v", err)
	}
	loaded, err := s.LoadBootstrapDeb(in.Version)
	if err != nil {
		t.Fatalf("LoadBootstrapDeb: %v", err)
	}
	if string(loaded) != string(body) {
		t.Fatal("bootstrap .deb roundtrip mismatch")
	}
}

func TestStore_AtomicRename_NoTrailingTmpFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSource(&SourceState{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "sources", ".tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files leaked: %v", matches)
	}
}
