//go:build integration

package reprepro

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegration_Local exercises the local-transport backend end to
// end against a real reprepro binary. Tagged `integration` so it
// stays out of the default test sweep; run with
//
//	go test -tags integration ./internal/drayman/backend/reprepro/
func TestIntegration_Local(t *testing.T) {
	if _, err := exec.LookPath("reprepro"); err != nil {
		t.Skip("reprepro not on PATH; skipping live integration")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not on PATH; skipping live integration")
	}

	basedir := t.TempDir()
	mustWrite(t, filepath.Join(basedir, "conf", "distributions"), `Origin: drayman-test
Label: drayman-test
Codename: stable
Architectures: amd64 arm64
Components: main
Description: drayman integration test
`)

	debPath := makeFixtureDeb(t, "hugo", "0.140.0", "amd64", "sha256:integration-hash-amd64")

	b := NewLocal(basedir, "stable", "main")
	ctx := context.Background()

	// Empty repo: hash should not exist, listing empty.
	if exists, err := b.HashExists(ctx, "sha256:integration-hash-amd64"); err != nil || exists {
		t.Fatalf("empty-repo HashExists: %v / %v", exists, err)
	}

	// Import.
	if err := b.Import(ctx, debPath); err != nil {
		t.Fatal(err)
	}

	// Cache was invalidated by Import; HashExists now sees the entry.
	exists, err := b.HashExists(ctx, "sha256:integration-hash-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected post-import HashExists=true")
	}

	// ListByNameArch returns the imported package.
	pkgs, err := b.ListByNameArch(ctx, "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].Version != "0.140.0" || pkgs[0].Architecture != "amd64" {
		t.Fatalf("ListByNameArch: %+v", pkgs)
	}
	if pkgs[0].BuildInputsHash != "sha256:integration-hash-amd64" {
		t.Errorf("BuildInputsHash: %q", pkgs[0].BuildInputsHash)
	}

	// Publish is a no-op for reprepro (already published atomically).
	if err := b.Publish(ctx); err != nil {
		t.Errorf("Publish: %v", err)
	}

	// Sanity-check the actual on-disk dists tree: Packages file
	// should exist and mention the hash.
	pkgFile := filepath.Join(basedir, "dists", "stable", "main", "binary-amd64", "Packages")
	body, err := os.ReadFile(pkgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "X-Cooper-Build-Inputs-Hash: sha256:integration-hash-amd64") {
		t.Errorf("Packages file missing expected hash field:\n%s", body)
	}
}

// makeFixtureDeb writes a minimal .deb to t.TempDir() with the given
// control fields and returns the path. Used by integration tests so
// they don't depend on cooper.
func makeFixtureDeb(t *testing.T, name, version, arch, hash string) string {
	t.Helper()
	root := t.TempDir()
	pkgRoot := filepath.Join(root, name+"-pkg")
	debianDir := filepath.Join(pkgRoot, "DEBIAN")
	usrBin := filepath.Join(pkgRoot, "usr", "bin")
	if err := os.MkdirAll(debianDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usrBin, 0o755); err != nil {
		t.Fatal(err)
	}
	control := "Package: " + name + "\n" +
		"Version: " + version + "\n" +
		"Section: utils\n" +
		"Priority: optional\n" +
		"Architecture: " + arch + "\n" +
		"Maintainer: Test <test@example.com>\n" +
		"Description: integration-test fixture\n" +
		"X-Cooper-Build-Inputs-Hash: " + hash + "\n"
	mustWrite(t, filepath.Join(debianDir, "control"), control)
	mustWrite(t, filepath.Join(usrBin, name), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(usrBin, name), 0o755); err != nil {
		t.Fatal(err)
	}
	debPath := filepath.Join(root, name+"_"+version+"_"+arch+".deb")
	cmd := exec.Command("dpkg-deb", "--build", "--root-owner-group", pkgRoot, debPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return debPath
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SSH exercises the ssh-transport backend against
// `ssh localhost` (assumes the test runner can ssh to localhost
// without password — set up the appropriate keys before running).
// Skipped automatically when ssh, scp, or reprepro is unavailable.
//
// Same scenario as TestIntegration_Local: empty repo → import →
// HashExists/ListByNameArch round-trip. Distinct distribution name
// so concurrent runs (e.g. local + ssh) don't share state.
func TestIntegration_SSH(t *testing.T) {
	if _, err := exec.LookPath("reprepro"); err != nil {
		t.Skip("reprepro not on PATH; skipping ssh integration")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skip("scp not on PATH")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not on PATH")
	}
	// Populate the user's known_hosts for localhost if absent so the
	// test doesn't depend on prior `ssh localhost` interaction.
	// ssh-keyscan output is appended; OpenSSH dedupes / hashes via the
	// hash-known-hosts setting in the user's config.
	hostKeyProbe := exec.Command("ssh-keyscan", "-H", "-T", "2", "localhost")
	if keyBytes, err := hostKeyProbe.Output(); err == nil && len(keyBytes) > 0 {
		khPath := os.ExpandEnv("$HOME/.ssh/known_hosts")
		f, ferr := os.OpenFile(khPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if ferr == nil {
			_, _ = f.Write(keyBytes)
			_ = f.Close()
		}
	}
	if err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=2", "localhost", "true").Run(); err != nil {
		t.Skipf("ssh localhost not configured for passwordless auth: %v", err)
	}

	// Per-run basedir under /tmp so the path is identical from both
	// the test process and the (same-host) ssh session.
	id := time.Now().UnixNano()
	basedir := filepath.Join("/tmp", "drayman-ssh-test", "base-"+stringFromInt64(id))
	t.Cleanup(func() { _ = os.RemoveAll(basedir) })

	mustWrite(t, filepath.Join(basedir, "conf", "distributions"), `Origin: drayman-test
Label: drayman-test
Codename: ssh-test
Architectures: amd64
Components: main
Description: drayman ssh integration test
`)

	debPath := makeFixtureDeb(t, "hugo", "0.140.0", "amd64", "sha256:ssh-integration-hash")

	b := NewSSH("localhost", basedir, "ssh-test", "main")
	ctx := context.Background()

	if exists, err := b.HashExists(ctx, "sha256:ssh-integration-hash"); err != nil || exists {
		t.Fatalf("empty-repo HashExists: %v / %v", exists, err)
	}

	if err := b.Import(ctx, debPath); err != nil {
		t.Fatal(err)
	}

	exists, err := b.HashExists(ctx, "sha256:ssh-integration-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected post-import HashExists=true over ssh")
	}

	pkgs, err := b.ListByNameArch(ctx, "hugo", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].BuildInputsHash != "sha256:ssh-integration-hash" {
		t.Errorf("post-import listing wrong: %+v", pkgs)
	}

	if err := b.Publish(ctx); err != nil {
		t.Errorf("Publish: %v", err)
	}
}

func stringFromInt64(n int64) string {
	const hexDigits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = hexDigits[n&0xf]
		n >>= 4
	}
	return string(buf[i:])
}
