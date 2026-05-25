package config

import (
	"strings"
	"testing"
)

func TestValidateMatrix_SimpleNoTemplate(t *testing.T) {
	cfg := &Config{
		Package: Package{Name: "foo-archive-keyring"},
		Keys: map[string]Key{
			"foo": {URL: "https://example.com/k"},
		},
		Sources: []Source{
			{ID: "foo", URIs: []string{"https://example.com"}, Suites: []string{"stable"}, Components: []string{"main"}, Key: "foo"},
		},
	}
	if err := ValidateMatrix(cfg); err != nil {
		t.Fatalf("simple/no-template should pass, got %v", err)
	}
}

func TestValidateMatrix_TemplateWithoutTargets(t *testing.T) {
	cfg := &Config{
		Package: Package{Name: "foo-archive-keyring"},
		Keys:    map[string]Key{"foo": {URL: "https://example.com/k"}},
		Sources: []Source{
			{ID: "foo", URIs: []string{"https://example.com"}, Suites: []string{"{{.Codename}}"}, Components: []string{"main"}, Key: "foo"},
		},
	}
	err := ValidateMatrix(cfg)
	if err == nil {
		t.Fatal("template token without targets should error")
	}
	if !strings.Contains(err.Error(), "without targets") {
		t.Errorf("error should mention missing targets, got %v", err)
	}
}

func TestValidateMatrix_MatrixWithoutNameTemplate(t *testing.T) {
	cfg := &Config{
		Package: Package{Name: "foo-archive-keyring"}, // no {{.Codename}}
		Keys:    map[string]Key{"foo": {URL: "https://example.com/k"}},
		Targets: []Target{
			{Distro: "debian", Codename: "bookworm"},
			{Distro: "debian", Codename: "trixie"},
		},
		Sources: []Source{
			{ID: "foo", URIs: []string{"https://example.com"}, Suites: []string{"{{.Codename}}"}, Components: []string{"main"}, Key: "foo"},
		},
	}
	err := ValidateMatrix(cfg)
	if err == nil {
		t.Fatal("matrix with non-templated name should error")
	}
	if !strings.Contains(err.Error(), "lacks a template token") {
		t.Errorf("error should mention name template requirement, got %v", err)
	}
}

func TestValidateMatrix_MatrixWithNameTemplate(t *testing.T) {
	cfg := &Config{
		Package: Package{Name: "foo-archive-keyring-{{.Codename}}"},
		Keys:    map[string]Key{"foo": {URL: "https://example.com/k"}},
		Targets: []Target{
			{Distro: "debian", Codename: "bookworm"},
			{Distro: "debian", Codename: "trixie"},
		},
		Sources: []Source{
			{ID: "foo", URIs: []string{"https://example.com"}, Suites: []string{"{{.Codename}}"}, Components: []string{"main"}, Key: "foo"},
		},
	}
	if err := ValidateMatrix(cfg); err != nil {
		t.Fatalf("templated name + matrix should pass, got %v", err)
	}
}

func TestValidateMatrix_SingleTargetNoTemplate(t *testing.T) {
	// targets: with a single entry shouldn't require a name template
	// — the .debs can't collide because there's only one of them.
	cfg := &Config{
		Package: Package{Name: "foo-archive-keyring"},
		Keys:    map[string]Key{"foo": {URL: "https://example.com/k"}},
		Targets: []Target{{Distro: "debian", Codename: "bookworm"}},
		Sources: []Source{
			{ID: "foo", URIs: []string{"https://example.com"}, Suites: []string{"stable"}, Components: []string{"main"}, Key: "foo"},
		},
	}
	if err := ValidateMatrix(cfg); err != nil {
		t.Fatalf("single-target no-template should pass, got %v", err)
	}
}
