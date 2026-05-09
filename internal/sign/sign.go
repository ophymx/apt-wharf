// Package sign loads the GPG signing key and emits InRelease (clearsigned)
// and Release.gpg (armored detached) signatures. The pubkey side is also
// exposed as bytes so the bootstrap builder and /pubkey.gpg endpoint can
// embed the same canonical keyring blob.
package sign

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Signer holds an unlocked signing entity and an optional next-pubkey entity
// (packaging-only, never used to sign).
type Signer struct {
	primary *openpgp.Entity
	nextPub *openpgp.Entity
}

// Load reads keyFile (binary or armored), decrypts with passphrase if non-nil,
// and optionally loads nextPubFile as a packaging-only second public key.
//
// The next pubkey must have a distinct primary fingerprint from the active
// signer — otherwise the rotation contract is meaningless.
func Load(keyFile string, passphrase []byte, nextPubFile string) (*Signer, error) {
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
	s := &Signer{primary: primary}

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
	// armor blocks start with `-----BEGIN PGP`; everything else is treated
	// as a binary OpenPGP packet stream.
	var keyring openpgp.EntityList
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

// Fingerprint returns the active primary key fingerprint.
func (s *Signer) Fingerprint() []byte { return s.primary.PrimaryKey.Fingerprint }

// Clearsign produces an armored clearsigned message — the InRelease format.
func (s *Signer) Clearsign(w io.Writer, message []byte) error {
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
func (s *Signer) DetachedSign(w io.Writer, message []byte) error {
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
func (s *Signer) KeyringBytes() []byte {
	var buf bytes.Buffer
	// entity.Serialize writes only public key material.
	_ = s.primary.Serialize(&buf)
	if s.nextPub != nil {
		_ = s.nextPub.Serialize(&buf)
	}
	return buf.Bytes()
}

// ArmoredKeyring is the same content as KeyringBytes but armored. Useful
// for human-readable troubleshooting; not used in the production path.
func (s *Signer) ArmoredKeyring() ([]byte, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := s.primary.Serialize(w); err != nil {
		return nil, err
	}
	if s.nextPub != nil {
		if err := s.nextPub.Serialize(w); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Zero best-effort wipes private key material. Go's runtime may have copied
// bytes elsewhere; this only handles the buffers we directly own.
func (s *Signer) Zero() {
	if s.primary != nil && s.primary.PrivateKey != nil {
		s.primary.PrivateKey = nil
		for i := range s.primary.Subkeys {
			s.primary.Subkeys[i].PrivateKey = nil
		}
	}
}
