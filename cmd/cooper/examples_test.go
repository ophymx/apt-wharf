package main

import (
	"path/filepath"
	"testing"
)

// TestExamplesValidateClean asserts every cooper.yaml under examples/
// passes `cooper validate`. Catches schema drift that would break the
// copy-paste starting points the README points users at.
func TestExamplesValidateClean(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/cooper.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no examples found; expected at least one")
	}
	for _, p := range matches {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			if err := cmdValidate([]string{p}); err != nil {
				t.Errorf("validate %s: %v", p, err)
			}
		})
	}
}
