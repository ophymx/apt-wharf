package sign

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// writeBinaryKey serializes a fresh entity to a tmp file in private form,
// returning the path. mode is 0600 so we can rewrite it; CheckSecureFile
// is exercised at the config layer, not here.
func writeBinaryKey(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secring.gpg")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := e.SerializePrivate(f, &packet.Config{}); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return path
}

func newTestEntity(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "test", name+"@example", &packet.Config{
		RSABits: 2048,
	})
	if err != nil {
		t.Fatalf("new entity: %v", err)
	}
	return e
}

func TestSigner_ClearsignAndDetached_Verify(t *testing.T) {
	e := newTestEntity(t, "Acme APT")
	keyPath := writeBinaryKey(t, e)

	s, err := Load(keyPath, nil, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	msg := []byte("Origin: Acme\nLabel: Acme APT\nSuite: stable\n")

	t.Run("clearsign roundtrip", func(t *testing.T) {
		var buf bytes.Buffer
		if err := s.Clearsign(&buf, msg); err != nil {
			t.Fatalf("Clearsign: %v", err)
		}
		out := buf.Bytes()
		if !bytes.HasPrefix(out, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
			t.Fatalf("clearsign output missing armor header: %q", out[:64])
		}
		block, _ := clearsign.Decode(out)
		if block == nil {
			t.Fatal("clearsign.Decode returned nil")
		}
		// Trailing newline normalization: clearsign appends \n if missing.
		if !bytes.Equal(bytes.TrimRight(block.Plaintext, "\n"), bytes.TrimRight(msg, "\n")) {
			t.Fatalf("plaintext mismatch:\n got=%q\nwant=%q", block.Plaintext, msg)
		}
		// Verify the signature against the keyring.
		kr := openpgp.EntityList{e}
		signer, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil)
		if err != nil {
			t.Fatalf("CheckDetachedSignature: %v", err)
		}
		if signer == nil {
			t.Fatal("expected non-nil signer")
		}
	})

	t.Run("detached sign roundtrip", func(t *testing.T) {
		var sig bytes.Buffer
		if err := s.DetachedSign(&sig, msg); err != nil {
			t.Fatalf("DetachedSign: %v", err)
		}
		if !bytes.HasPrefix(sig.Bytes(), []byte("-----BEGIN PGP SIGNATURE-----")) {
			t.Fatalf("detached sig missing armor header")
		}
		kr := openpgp.EntityList{e}
		_, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(msg), &sig, nil)
		if err != nil {
			t.Fatalf("verify detached: %v", err)
		}
	})

	t.Run("keyring bytes parse back", func(t *testing.T) {
		kr := s.KeyringBytes()
		if len(kr) == 0 {
			t.Fatal("empty keyring bytes")
		}
		entities, err := openpgp.ReadKeyRing(bytes.NewReader(kr))
		if err != nil {
			t.Fatalf("ReadKeyRing: %v", err)
		}
		if len(entities) != 1 {
			t.Fatalf("got %d entities, want 1", len(entities))
		}
	})
}

func TestSigner_NextPubkey(t *testing.T) {
	primary := newTestEntity(t, "Active")
	next := newTestEntity(t, "Next")

	primaryPath := writeBinaryKey(t, primary)

	// next pub: write only public side
	dir := t.TempDir()
	nextPath := filepath.Join(dir, "next.pub")
	f, err := os.Create(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Serialize(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s, err := Load(primaryPath, nil, nextPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	kr := s.KeyringBytes()
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(kr))
	if err != nil {
		t.Fatalf("ReadKeyRing: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("expected 2 keys in keyring, got %d", len(entities))
	}
	// Canonical order: active first.
	if !bytes.Equal(entities[0].PrimaryKey.Fingerprint, primary.PrimaryKey.Fingerprint) {
		t.Fatal("active key should come first in keyring")
	}
}

func TestSigner_NextPubkeyDuplicateFingerprint(t *testing.T) {
	e := newTestEntity(t, "Same")
	primaryPath := writeBinaryKey(t, e)

	dir := t.TempDir()
	nextPath := filepath.Join(dir, "next.pub")
	f, err := os.Create(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = Load(primaryPath, nil, nextPath)
	if err == nil {
		t.Fatal("expected error on duplicate fingerprints")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("error did not mention fingerprint: %v", err)
	}
}
