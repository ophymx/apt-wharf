package sign

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureKey_GeneratesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "secring.gpg")

	if err := EnsureKey(path, "Local Ops <ops@localhost>"); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o400 {
		t.Fatalf("perm = %#o, want 0400", perm)
	}

	// Loadable + signable round-trip.
	s, err := Load(path, nil, "")
	if err != nil {
		t.Fatalf("Load generated key: %v", err)
	}
	var sig bytes.Buffer
	if err := s.DetachedSign(&sig, []byte("hello")); err != nil {
		t.Fatalf("DetachedSign: %v", err)
	}
	if !bytes.HasPrefix(sig.Bytes(), []byte("-----BEGIN PGP SIGNATURE-----")) {
		t.Fatal("detached sig missing armor header")
	}
}

func TestEnsureKey_NoopWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secring.gpg")

	if err := EnsureKey(path, "First Owner <a@b>"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)

	// Different uid, but the file already exists → no-op.
	if err := EnsureKey(path, "Second Owner <c@d>"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)

	if !bytes.Equal(first, second) {
		t.Fatal("EnsureKey replaced an existing key file; expected no-op")
	}
}

func TestEnsureKey_RejectsEmptyUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secring.gpg")
	err := EnsureKey(path, "")
	if err == nil {
		t.Fatal("expected error on empty uid")
	}
}
