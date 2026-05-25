package main

import (
	"path/filepath"
	"testing"
)

// TestExamplesValidateClean asserts every chandler.yaml under
// examples/ passes `chandler validate`. Mirrors cooper's and staves's
// regression nets — schema drift trips at test time. Skips when no
// chandler examples exist yet.
func TestExamplesValidateClean(t *testing.T) {
	matches, err := filepath.Glob("../../examples/chandler/*/chandler.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("no chandler examples found; nothing to validate")
	}
	for _, p := range matches {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			if err := cmdValidate([]string{p}); err != nil {
				t.Errorf("validate %s: %v", p, err)
			}
		})
	}
}
