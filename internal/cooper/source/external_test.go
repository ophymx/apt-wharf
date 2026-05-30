package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
)

// writeScript drops body into <dir>/discover.sh with 0755 perms. The
// returned path is what config.ExternalSource.Command[0] should point
// at (relative to dir, so "./discover.sh"; absolute resolution is
// driven by ResolveExternal's workdir argument).
func writeScript(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "discover.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func TestResolveExternal_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
cat <<'EOF'
{
  "version": "1.2.3",
  "assets": [
    { "arch": "amd64", "url": "https://example.invalid/foo-1.2.3-amd64.tar.gz", "sha256": "0000000000000000000000000000000000000000000000000000000000000001" }
  ]
}
EOF
`)
	res, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err != nil {
		t.Fatalf("ResolveExternal: %v", err)
	}
	if res.Version != "1.2.3" {
		t.Errorf("Version: %q, want 1.2.3", res.Version)
	}
	if got := res.Assets["amd64"].URL; got != "https://example.invalid/foo-1.2.3-amd64.tar.gz" {
		t.Errorf("amd64.URL: %q", got)
	}
	if got := res.Assets["amd64"].SHA256; got != "0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("amd64.SHA256: %q", got)
	}
	if res.SourceEpoch <= 0 {
		t.Errorf("SourceEpoch: %d", res.SourceEpoch)
	}
}

func TestResolveExternal_NoSHA256(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo '{"version":"1.0.0","assets":[{"arch":"amd64","url":"https://example.invalid/a.tar.gz"}]}'
`)
	res, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err != nil {
		t.Fatalf("ResolveExternal: %v", err)
	}
	if got := res.Assets["amd64"].SHA256; got != "" {
		t.Errorf("SHA256 unexpectedly set: %q", got)
	}
}

func TestResolveExternal_MissingVersion(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo '{"assets":[{"arch":"amd64","url":"https://example.invalid/a.tar.gz"}]}'
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "missing required .version") {
		t.Errorf("expected missing-version error, got %v", err)
	}
}

func TestResolveExternal_BadURLScheme(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo '{"version":"1.0.0","assets":[{"arch":"amd64","url":"file:///etc/passwd"}]}'
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "must use http or https scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestResolveExternal_BadSHA256(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo '{"version":"1.0.0","assets":[{"arch":"amd64","url":"https://example.invalid/a.tar.gz","sha256":"deadbeef"}]}'
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "expected 64 hex chars") {
		t.Errorf("expected sha256 length error, got %v", err)
	}
}

func TestResolveExternal_ExitNonzero(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo "vendor URL returned 503" >&2
exit 7
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit 7") {
		t.Errorf("error missing exit code: %v", err)
	}
	if !strings.Contains(err.Error(), "vendor URL returned 503") {
		t.Errorf("error missing stderr tail: %v", err)
	}
}

func TestResolveExternal_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo "this is not json"
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "parse stdout") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestResolveExternal_DuplicateArch(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo '{"version":"1.0.0","assets":[
  {"arch":"amd64","url":"https://example.invalid/a.tar.gz"},
  {"arch":"amd64","url":"https://example.invalid/b.tar.gz"}
]}'
`)
	_, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate arch") {
		t.Errorf("expected duplicate-arch error, got %v", err)
	}
}

func TestResolveExternal_EnvPassedThrough(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, `#!/bin/sh
echo "{\"version\":\"$FOO\",\"assets\":[{\"arch\":\"amd64\",\"url\":\"https://example.invalid/x.tar.gz\"}]}"
`)
	res, err := ResolveExternal(context.Background(), &config.ExternalSource{
		Command: []string{"./discover.sh"},
		Env:     map[string]string{"FOO": "9.9.9"},
	}, dir)
	if err != nil {
		t.Fatalf("ResolveExternal: %v", err)
	}
	if res.Version != "9.9.9" {
		t.Errorf("Version: %q, want 9.9.9 (env not threaded through)", res.Version)
	}
}
