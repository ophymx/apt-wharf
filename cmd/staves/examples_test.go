package main

import (
	"path/filepath"
	"testing"
)

// TestExamplesValidateClean asserts every staves.yaml under
// examples/ passes `staves validate`. Mirrors cooper's regression
// net so schema drift catches at test time.
func TestExamplesValidateClean(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/staves.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("no staves examples found; nothing to validate")
	}
	for _, p := range matches {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			if err := cmdValidate([]string{p}); err != nil {
				t.Errorf("validate %s: %v", p, err)
			}
		})
	}
}
