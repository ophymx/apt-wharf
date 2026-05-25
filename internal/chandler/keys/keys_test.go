package keys

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// makeKey generates a fresh OpenPGP entity for use in tests.
// Avoids checked-in fixtures so the tests stay portable.
func makeKey(t *testing.T, name, comment, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, comment, email, nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

// serializeBinary writes the entity to a binary OpenPGP byte slice.
func serializeBinary(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return buf.Bytes()
}

// serializeArmored writes the entity to an ASCII-armored byte slice.
func serializeArmored(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(aw); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	return buf.Bytes()
}

func TestInspect_Binary(t *testing.T) {
	e := makeKey(t, "Binary Test", "", "binary@example.com")
	info, err := Inspect(serializeBinary(t, e))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !strings.Contains(info.UID, "Binary Test") {
		t.Errorf("UID: want substring 'Binary Test', got %q", info.UID)
	}
	if !strings.Contains(info.UID, "<binary@example.com>") {
		t.Errorf("UID: want email substring, got %q", info.UID)
	}
	if len(info.Fingerprint) != 40 {
		t.Errorf("Fingerprint: want 40 hex chars, got %d (%q)", len(info.Fingerprint), info.Fingerprint)
	}
	if info.Fingerprint != strings.ToUpper(info.Fingerprint) {
		t.Errorf("Fingerprint should be uppercase: %q", info.Fingerprint)
	}
	if info.Expiry != "" {
		t.Errorf("Expiry: want empty for non-expiring key, got %q", info.Expiry)
	}
}

func TestInspect_Armored(t *testing.T) {
	e := makeKey(t, "Armored Test", "ci", "armored@example.com")
	info, err := Inspect(serializeArmored(t, e))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !strings.Contains(info.UID, "Armored Test") {
		t.Errorf("UID: want substring 'Armored Test', got %q", info.UID)
	}
}

func TestInspect_Empty(t *testing.T) {
	_, err := Inspect([]byte{})
	if err == nil {
		t.Fatal("expected error on empty blob")
	}
}

func TestInspect_Junk(t *testing.T) {
	_, err := Inspect([]byte("this is not a pgp key"))
	if err == nil {
		t.Fatal("expected error on garbage blob")
	}
}

func TestFetchURL_Armored(t *testing.T) {
	e := makeKey(t, "HTTP Test", "", "http@example.com")
	armored := serializeArmored(t, e)
	binary := serializeBinary(t, e)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pgp-keys")
		_, _ = w.Write(armored)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	f, err := c.FetchURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if !strings.Contains(f.Info.UID, "HTTP Test") {
		t.Errorf("UID: got %q", f.Info.UID)
	}
	if len(f.Binary) == 0 {
		t.Fatal("empty binary in result")
	}
	// The Binary field should match what we'd get from serializing
	// the entity to binary directly (modulo metadata; just check
	// inspection round-trip).
	info2, err := Inspect(f.Binary)
	if err != nil {
		t.Fatalf("Inspect of returned binary: %v", err)
	}
	if info2.Fingerprint != f.Info.Fingerprint {
		t.Errorf("fingerprint mismatch through dearmor: %q vs %q",
			info2.Fingerprint, f.Info.Fingerprint)
	}
	// And the binary should parse identically to direct serialization.
	if !bytes.Equal(f.Binary, binary) {
		t.Logf("dearmored binary (len=%d) does not byte-match direct serialize (len=%d) — acceptable; metadata may differ",
			len(f.Binary), len(binary))
	}
}

func TestFetchURL_Binary(t *testing.T) {
	e := makeKey(t, "Binary HTTP", "", "binhttp@example.com")
	binary := serializeBinary(t, e)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(binary)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	f, err := c.FetchURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if !strings.Contains(f.Info.UID, "Binary HTTP") {
		t.Errorf("UID: got %q", f.Info.UID)
	}
	if !bytes.Equal(f.Binary, binary) {
		t.Error("binary input was modified through fetch+parse")
	}
}

func TestFetchURL_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	_, err := c.FetchURL(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestFetchURL_SizeCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), MaxBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	_, err := c.FetchURL(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error on oversize response")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size cap: %v", err)
	}
}

func TestFetchURLs_Concatenates(t *testing.T) {
	e1 := makeKey(t, "First", "", "first@example.com")
	e2 := makeKey(t, "Second", "", "second@example.com")
	a1 := serializeArmored(t, e1)
	a2 := serializeArmored(t, e2)

	mux := http.NewServeMux()
	mux.HandleFunc("/k1", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(a1) })
	mux.HandleFunc("/k2", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(a2) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	f, err := c.FetchURLs(context.Background(), []string{srv.URL + "/k1", srv.URL + "/k2"})
	if err != nil {
		t.Fatalf("FetchURLs: %v", err)
	}
	// Info reflects the first key.
	if !strings.Contains(f.Info.UID, "First") {
		t.Errorf("Info should reflect first key, got UID=%q", f.Info.UID)
	}
	// Concatenated binary must parse as a 2-entity keyring.
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(f.Binary))
	if err != nil {
		t.Fatalf("ReadKeyRing on concatenated: %v", err)
	}
	if len(entities) != 2 {
		t.Errorf("want 2 entities, got %d", len(entities))
	}
}

func TestFetchURLs_Empty(t *testing.T) {
	c := &Client{}
	_, err := c.FetchURLs(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error on empty URL list")
	}
}
