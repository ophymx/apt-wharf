// Package sign produces the OpenPGP material apt-signpost embeds in
// Release.gpg, InRelease, and the bootstrap .deb. Two backends share the
// Signer interface:
//
//   - internal — in-process signing via ProtonMail go-crypto; see Load.
//   - external — exec a configurable command per signing op; see
//     LoadExternal. Useful when the signing key is held in Vault, an HSM,
//     or anywhere else off-host; the daemon stays a non-secret-holder.
//
// KeyringBytes() must be byte-stable across calls and reload — the bootstrap
// .deb's input hash mixes it in, and any drift makes apt see a phantom
// upgrade every tick. See stability_test.go.
package sign

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Signer is the abstraction the refresher depends on. Implementations must
// be safe for concurrent use — Refresher.tickMu serializes refresh ticks so
// the only real concurrency Signer faces is KeyringBytes() reads from the
// HTTP handlers; that returns a cached slice so a no-op lock suffices.
type Signer interface {
	// Clearsign writes an armored clearsigned envelope of message to w —
	// the wire format of InRelease.
	Clearsign(w io.Writer, message []byte) error

	// DetachedSign writes an armored detached signature of message to w —
	// the wire format of Release.gpg.
	DetachedSign(w io.Writer, message []byte) error

	// KeyringBytes returns the binary OpenPGP public keyring served at
	// /pubkey.gpg and embedded under /usr/share/keyrings/<repo>.gpg in the
	// bootstrap .deb. Result must be deterministic across calls and reload.
	KeyringBytes() []byte

	// Zero best-effort wipes any in-memory secret material the signer owns.
	// External signers carry no secret material and may no-op.
	Zero()
}

// internalSigner holds an unlocked signing entity and an optional
// next-pubkey entity (packaging-only, never used to sign). It implements
// the Signer interface; the constructor is Load.
type internalSigner struct {
	primary *openpgp.Entity
	nextPub *openpgp.Entity
}

// Load reads keyFile (binary or armored), decrypts with passphrase if non-nil,
// and optionally loads nextPubFile as a packaging-only second public key.
//
// The next pubkey must have a distinct primary fingerprint from the active
// signer — otherwise the rotation contract is meaningless.
func Load(keyFile string, passphrase []byte, nextPubFile string) (Signer, error) {
	primary, err := readEntity(keyFile)
	if err != nil {
		return nil, fmt.Errorf("signing.key_file %s: %w", keyFile, err)
	}
	if primary.PrivateKey == nil {
		return nil, fmt.Errorf("signing.key_file %s: contains no secret key", keyFile)
	}
	if err := unlock(primary, passphrase); err != nil {
		return nil, fmt.Errorf("signing.key_file %s: %w", keyFile, err)
	}
	s := &internalSigner{primary: primary}

	if nextPubFile != "" {
		next, err := readEntity(nextPubFile)
		if err != nil {
			return nil, fmt.Errorf("signing.next_pubkey_file %s: %w", nextPubFile, err)
		}
		if bytes.Equal(primary.PrimaryKey.Fingerprint, next.PrimaryKey.Fingerprint) {
			return nil, fmt.Errorf("signing.next_pubkey_file %s has the same fingerprint as signing.key_file — rotation requires distinct keys", nextPubFile)
		}
		s.nextPub = next
	}
	return s, nil
}

func readEntity(path string) (*openpgp.Entity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseEntity(raw)
}

// parseEntity decodes one OpenPGP entity from raw bytes. Accepts either
// armored or binary input; rejects multi-key blobs because the caller
// always wants a single, unambiguous primary.
func parseEntity(raw []byte) (*openpgp.Entity, error) {
	// armor blocks start with `-----BEGIN PGP`; everything else is treated
	// as a binary OpenPGP packet stream.
	var keyring openpgp.EntityList
	var err error
	if bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("-----BEGIN PGP")) {
		keyring, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	} else {
		keyring, err = openpgp.ReadKeyRing(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, fmt.Errorf("parse keyring: %w", err)
	}
	if len(keyring) == 0 {
		return nil, errors.New("no keys found")
	}
	if len(keyring) > 1 {
		return nil, fmt.Errorf("file contains %d keys, expected exactly 1", len(keyring))
	}
	return keyring[0], nil
}

func unlock(e *openpgp.Entity, passphrase []byte) error {
	if e.PrivateKey != nil && e.PrivateKey.Encrypted {
		if len(passphrase) == 0 {
			return errors.New("private key is encrypted but no passphrase configured")
		}
		if err := e.PrivateKey.Decrypt(passphrase); err != nil {
			return fmt.Errorf("decrypt primary key: %w", err)
		}
	}
	for i := range e.Subkeys {
		sk := &e.Subkeys[i]
		if sk.PrivateKey != nil && sk.PrivateKey.Encrypted {
			if err := sk.PrivateKey.Decrypt(passphrase); err != nil {
				return fmt.Errorf("decrypt subkey %x: %w", sk.PublicKey.Fingerprint, err)
			}
		}
	}
	return nil
}

// Clearsign produces an armored clearsigned message — the InRelease format.
func (s *internalSigner) Clearsign(w io.Writer, message []byte) error {
	cfg := &packet.Config{DefaultHash: crypto.SHA256}
	plaintext, err := clearsign.Encode(w, s.primary.PrivateKey, cfg)
	if err != nil {
		return fmt.Errorf("clearsign init: %w", err)
	}
	if _, err := plaintext.Write(message); err != nil {
		return fmt.Errorf("clearsign write: %w", err)
	}
	if err := plaintext.Close(); err != nil {
		return fmt.Errorf("clearsign close: %w", err)
	}
	return nil
}

// DetachedSign produces an armored detached signature — the Release.gpg format.
func (s *internalSigner) DetachedSign(w io.Writer, message []byte) error {
	cfg := &packet.Config{DefaultHash: crypto.SHA256}
	if err := openpgp.ArmoredDetachSign(w, s.primary, bytes.NewReader(message), cfg); err != nil {
		return fmt.Errorf("detached sign: %w", err)
	}
	return nil
}

// KeyringBytes returns the binary public keyring containing the active key
// and (if configured) the next pubkey, in canonical order: active first,
// next second. The bootstrap .deb embeds these bytes verbatim, and the
// /pubkey.gpg HTTP endpoint serves them.
func (s *internalSigner) KeyringBytes() []byte {
	var buf bytes.Buffer
	// entity.Serialize writes only public key material.
	_ = s.primary.Serialize(&buf)
	if s.nextPub != nil {
		_ = s.nextPub.Serialize(&buf)
	}
	return buf.Bytes()
}

// Zero best-effort wipes private key material. Go's runtime may have copied
// bytes elsewhere; this only handles the buffers we directly own.
func (s *internalSigner) Zero() {
	if s.primary != nil && s.primary.PrivateKey != nil {
		s.primary.PrivateKey = nil
		for i := range s.primary.Subkeys {
			s.primary.Subkeys[i].PrivateKey = nil
		}
	}
}
