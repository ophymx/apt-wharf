package sign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// If KeyringBytes is non-deterministic, the bootstrap input hash changes
// every refresh tick and apt sees a fake "upgrade available". Pin it.
func TestSigner_KeyringBytesIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secring.gpg")
	if err := EnsureKey(path, "Local <local@localhost>"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	first := s.KeyringBytes()
	second := s.KeyringBytes()
	if !bytes.Equal(first, second) {
		t.Fatalf("KeyringBytes is not deterministic across calls\n  first  sha256=%s len=%d\n  second sha256=%s len=%d",
			hex.EncodeToString(sha256Bytes(first)), len(first),
			hex.EncodeToString(sha256Bytes(second)), len(second))
	}
}

// And across reload — i.e., daemon restart should produce the same bytes.
func TestSigner_KeyringBytesIsStableAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secring.gpg")
	if err := EnsureKey(path, "Local <local@localhost>"); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o400)

	s1, err := Load(path, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s1.KeyringBytes(), s2.KeyringBytes()) {
		t.Fatal("KeyringBytes differs across two Loads of the same file")
	}
}

func sha256Bytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
