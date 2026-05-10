package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTop_Happy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packages/hugo.yaml", "---\n---\n")
	writeFile(t, dir, "packages/terraform.yaml", "---\n---\n")

	cfg := writeFile(t, dir, "cooper.yaml", `
github:
  token_env: GITHUB_TOKEN
packages:
  - ./packages/hugo.yaml
  - ./packages/terraform.yaml
`)
	got, err := LoadTop(cfg)
	if err != nil {
		t.Fatalf("LoadTop: %v", err)
	}
	if got.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("token_env: %s", got.GitHub.TokenEnv)
	}
	if got.GitHub.TokenFile != "" {
		t.Errorf("token_file should be empty: %s", got.GitHub.TokenFile)
	}
	if len(got.Packages) != 2 {
		t.Fatalf("packages: %v", got.Packages)
	}
	for _, p := range got.Packages {
		if !filepath.IsAbs(p) {
			t.Errorf("package path not absolute: %s", p)
		}
	}
	if got.Dir != dir {
		t.Errorf("Dir: got %s want %s", got.Dir, dir)
	}
}

func TestLoadTop_AbsolutePackagePath(t *testing.T) {
	dir := t.TempDir()
	pkg := writeFile(t, dir, "external/hugo.yaml", "---\n---\n")
	cfg := writeFile(t, dir, "cooper.yaml", "packages:\n  - "+pkg+"\n")
	got, err := LoadTop(cfg)
	if err != nil {
		t.Fatalf("LoadTop: %v", err)
	}
	if got.Packages[0] != pkg {
		t.Errorf("absolute path mangled: got %s want %s", got.Packages[0], pkg)
	}
}

func TestLoadTop_Errors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packages/hugo.yaml", "---\n---\n")

	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "no packages",
			body:    "github: { token_env: GITHUB_TOKEN }\n",
			wantSub: "must list at least one path",
		},
		{
			name:    "empty packages",
			body:    "packages: []\n",
			wantSub: "must list at least one path",
		},
		{
			name:    "duplicate package path",
			body:    "packages:\n  - ./packages/hugo.yaml\n  - ./packages/hugo.yaml\n",
			wantSub: "duplicates an earlier entry",
		},
		{
			name:    "non-existent package path",
			body:    "packages:\n  - ./packages/missing.yaml\n",
			wantSub: "missing.yaml",
		},
		{
			name: "token_env and token_file mutually exclusive",
			body: `
github:
  token_env: GITHUB_TOKEN
  token_file: /etc/cooper.token
packages:
  - ./packages/hugo.yaml
`,
			wantSub: "mutually exclusive",
		},
		{
			name:    "unknown top-level key",
			body:    "packages: [./packages/hugo.yaml]\nbogus: 1\n",
			wantSub: "field bogus not found",
		},
		{
			name:    "second YAML document rejected",
			body:    "packages: [./packages/hugo.yaml]\n---\nfoo: 1\n",
			wantSub: "single YAML document",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeFile(t, dir, "cooper.yaml", tc.body)
			_, err := LoadTop(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
