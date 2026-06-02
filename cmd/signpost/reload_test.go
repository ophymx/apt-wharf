package main

import (
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/signpost/config"
)

func baseCfg() *config.Config {
	return &config.Config{
		Server: config.Server{Listen: "127.0.0.1:8080"},
		Paths:  config.Paths{StateDir: "/var/lib/signpost/state"},
	}
}

func TestAssertHotReloadable_NoChangeIsOK(t *testing.T) {
	a := baseCfg()
	b := baseCfg()
	if err := assertHotReloadable(a, b); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestAssertHotReloadable_ListenChangeRejected(t *testing.T) {
	a := baseCfg()
	b := baseCfg()
	b.Server.Listen = "0.0.0.0:8080"
	err := assertHotReloadable(a, b)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server.listen changed") {
		t.Errorf("err = %v, want mention of server.listen", err)
	}
}

func TestAssertHotReloadable_StateDirChangeRejected(t *testing.T) {
	a := baseCfg()
	b := baseCfg()
	b.Paths.StateDir = "/srv/signpost"
	err := assertHotReloadable(a, b)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "paths.state_dir changed") {
		t.Errorf("err = %v, want mention of paths.state_dir", err)
	}
}

func TestAssertHotReloadable_BothChangedJoined(t *testing.T) {
	a := baseCfg()
	b := baseCfg()
	b.Server.Listen = ":9090"
	b.Paths.StateDir = "/srv/signpost"
	err := assertHotReloadable(a, b)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "server.listen") || !strings.Contains(msg, "paths.state_dir") {
		t.Errorf("err = %v, want both fields mentioned", err)
	}
}

func TestAssertHotReloadable_OtherFieldsAllowed(t *testing.T) {
	// Sanity: confirm we don't reject changes to fields that ARE hot-swappable.
	a := baseCfg()
	a.Repository.BaseURL = "https://old.example"
	a.Refresh.Interval = config.Duration(0)
	b := baseCfg()
	b.Repository.BaseURL = "https://new.example"
	b.Refresh.Interval = config.Duration(0)
	if err := assertHotReloadable(a, b); err != nil {
		t.Fatalf("expected nil for hot-swappable change, got %v", err)
	}
}
