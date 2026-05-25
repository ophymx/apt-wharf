package config

import (
	"fmt"
	"slices"
	"strings"
)

// ValidateMatrix checks the matrix-template invariants documented in
// chandler-design.md's "Templating (matrix mode)" section. Run after
// Load — the rules here couple the keys/sources schema (validated by
// Load) to the targets schema (also validated by Load).
//
// Two rules are enforced:
//
//  1. Template tokens ({{...}}) anywhere in a templated field require
//     targets: to be defined. Otherwise there's no variable set to
//     substitute against.
//  2. With > 1 target, package.name must contain a template token —
//     otherwise every .deb in the matrix would collide on filename.
//
// A third design rule — "targets defined but no field varies across
// them" — is intentionally not implemented in v0: in practice it
// overlaps with rule (2) and the user-visible failure mode (collision
// on resolved name) is the same.
func ValidateMatrix(cfg *Config) error {
	templated := anyTemplated(cfg)
	if templated && len(cfg.Targets) == 0 {
		return fmt.Errorf("template tokens used without targets: defined; add a targets: block or remove the {{...}} markers")
	}
	if len(cfg.Targets) > 1 && !HasTemplateToken(cfg.Package.Name) {
		return fmt.Errorf("matrix has %d targets but package.name %q lacks a template token; resulting .debs would collide on filename", len(cfg.Targets), cfg.Package.Name)
	}
	return nil
}

// HasTemplateToken reports whether s contains a Go template action
// marker. Cheap syntactic check; doesn't parse the template.
func HasTemplateToken(s string) bool {
	return strings.Contains(s, "{{")
}

func anyTemplated(cfg *Config) bool {
	if HasTemplateToken(cfg.Package.Name) {
		return true
	}
	for _, k := range cfg.Keys {
		if HasTemplateToken(k.URL) {
			return true
		}
		if slices.ContainsFunc(k.URLs, HasTemplateToken) {
			return true
		}
	}
	for _, s := range cfg.Sources {
		for _, xs := range [][]string{s.URIs, s.Suites, s.Components, s.Architectures, s.Types} {
			if slices.ContainsFunc(xs, HasTemplateToken) {
				return true
			}
		}
	}
	return false
}
