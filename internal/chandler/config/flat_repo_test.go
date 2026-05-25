package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsFlatRepo(t *testing.T) {
	cases := []struct {
		name   string
		suites []string
		want   bool
	}{
		{"single codename", []string{"bookworm"}, false},
		{"multiple codenames", []string{"bookworm", "trixie"}, false},
		{"flat root", []string{"/"}, true},
		{"flat path", []string{"stable/"}, true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsFlatRepo(c.suites); got != c.want {
				t.Errorf("IsFlatRepo(%v) = %v, want %v", c.suites, got, c.want)
			}
		})
	}
}

// writeTemp writes YAML to a temp file and returns the path.
func writeTemp(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chandler.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_FlatRepoAcceptsEmptyComponents(t *testing.T) {
	yaml := `
package:
  name: nvidia-cuda-archive-keyring
  version: "1.0"
  description: x
  maintainer: x
keys:
  nvidia:
    url: https://example.com/key
sources:
  - id: nvidia
    uris: https://example.com/repos/debian12/x86_64/
    suites: /
    architectures: [amd64]
    key: nvidia
`
	if _, err := Load(writeTemp(t, yaml)); err != nil {
		t.Fatalf("flat repo with no components should load, got %v", err)
	}
}

func TestLoad_FlatRepoRejectsComponents(t *testing.T) {
	yaml := `
package:
  name: foo
  version: "1.0"
  description: x
  maintainer: x
keys:
  k:
    url: https://example.com/key
sources:
  - id: foo
    uris: https://example.com/
    suites: /
    components: main
    key: k
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("flat repo with components should reject")
	}
	if !strings.Contains(err.Error(), "components must be absent") {
		t.Errorf("error should mention components-must-be-absent rule, got %v", err)
	}
}

func TestLoad_CodenameRepoRequiresComponents(t *testing.T) {
	yaml := `
package:
  name: foo
  version: "1.0"
  description: x
  maintainer: x
keys:
  k:
    url: https://example.com/key
sources:
  - id: foo
    uris: https://example.com/
    suites: bookworm
    key: k
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("codename suite without components should reject")
	}
	if !strings.Contains(err.Error(), "components is required") {
		t.Errorf("error should mention components-required rule, got %v", err)
	}
}
