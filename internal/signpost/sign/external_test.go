package sign

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// TestMain lets the test binary double as a sign helper. When
// SIGN_HELPER_MODE=1 it dispatches to runSignHelper instead of the
// test runner. Shim scripts in the tests exec the test binary in this
// mode to perform real OpenPGP ops without rolling a separate Go
// helper binary.
func TestMain(m *testing.M) {
	if os.Getenv("SIGN_HELPER_MODE") == "1" {
		runSignHelper()
		return
	}
	os.Exit(m.Run())
}

// runSignHelper reads argv as: <op> <secring-path> <in-path> <out-path>
// where op is "detach-sign" or "clear-sign". Loads the secret keyring
// in-process and writes the signed output. Exits the process directly.
func runSignHelper() {
	args := os.Args[1:]
	if len(args) != 4 {
		fmt.Fprintf(os.Stderr, "sign-helper: want 4 args (op secring in out), got %d\n", len(args))
		os.Exit(2)
	}
	op, secring, inPath, outPath := args[0], args[1], args[2], args[3]
	signer, err := Load(secring, nil, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign-helper: load: %v\n", err)
		os.Exit(1)
	}
	msg, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign-helper: read in: %v\n", err)
		os.Exit(1)
	}
	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign-helper: create out: %v\n", err)
		os.Exit(1)
	}
	switch op {
	case "detach-sign":
		err = signer.DetachedSign(out, msg)
	case "clear-sign":
		err = signer.Clearsign(out, msg)
	default:
		fmt.Fprintf(os.Stderr, "sign-helper: unknown op %q\n", op)
		os.Exit(2)
	}
	if cerr := out.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign-helper: %s: %v\n", op, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// buildSignHelper writes a wrapper script that re-execs the running test
// binary in SIGN_HELPER_MODE. The shim used by TestExternal_DetachSignAndClearsign
// then execs this wrapper with `<op> <secring> <in> <out>` argv.
func buildSignHelper(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	path := filepath.Join(dir, "sign-helper.sh")
	body := "#!/bin/sh\nexec env SIGN_HELPER_MODE=1 " + self + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write sign-helper: %v", err)
	}
	return path
}

// makeShim writes a tiny POSIX shell script in dir, returns its absolute
// path. body is appended after the shebang. Skips on non-POSIX hosts.
func makeShim(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("external signer tests use POSIX shell scripts")
	}
	path := filepath.Join(dir, "shim.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return path
}

// writeArmoredPubkey serializes e.PublicKey to an armored .asc file and
// returns the path. The exported half — same shape vault-pgp-sign's
// `export` subcommand emits.
func writeArmoredPubkey(t *testing.T, e *openpgp.Entity, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create pubkey: %v", err)
	}
	defer f.Close()
	// Pubkey only: entity.Serialize emits public material.
	// The simplest armored form goes via openpgp.ArmoredDetachSign's
	// keyring writer; here we just write binary and let parseEntity
	// accept either form — armored test exercised separately.
	if err := e.Serialize(f); err != nil {
		t.Fatalf("serialize pubkey: %v", err)
	}
	return path
}

// writeSecring serializes e (private + public) into dir/secring.gpg and
// returns the absolute path. Mirrors writeBinaryKey but accepts an
// explicit dir so the test can co-locate it with the shim it'll exec.
func writeSecring(t *testing.T, e *openpgp.Entity, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "secring.gpg")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open secring: %v", err)
	}
	defer f.Close()
	if err := e.SerializePrivate(f, nil); err != nil {
		t.Fatalf("serialize secring: %v", err)
	}
	return path
}

func TestExternal_PubkeyFile(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "PubkeyFile Test")
	pub := writeArmoredPubkey(t, e, dir, "release.pub")

	shim := makeShim(t, dir, `exit 99`) // never invoked: pubkey_file overrides export
	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command:    []string{shim},
		PubkeyFile: pub,
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	kr := s.KeyringBytes()
	if len(kr) == 0 {
		t.Fatal("empty keyring bytes")
	}
	got, err := openpgp.ReadKeyRing(bytes.NewReader(kr))
	if err != nil {
		t.Fatalf("ReadKeyRing: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d keys, want 1", len(got))
	}
	if !bytes.Equal(got[0].PrimaryKey.Fingerprint, e.PrimaryKey.Fingerprint) {
		t.Fatal("keyring fingerprint differs from pubkey_file source")
	}
}

func TestExternal_ExportSubcommand(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "Export Test")
	pubPath := writeArmoredPubkey(t, e, dir, "release.pub")

	// Shim's `export` subcommand: cat the bytes to stdout.
	shim := makeShim(t, dir, `
case "$1" in
  export) cat `+pubPath+` ;;
  *) echo "unsupported: $1" >&2; exit 2 ;;
esac
`)

	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command: []string{shim},
		Key:     "release-2025",
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	got, err := openpgp.ReadKeyRing(bytes.NewReader(s.KeyringBytes()))
	if err != nil {
		t.Fatalf("ReadKeyRing: %v", err)
	}
	if !bytes.Equal(got[0].PrimaryKey.Fingerprint, e.PrimaryKey.Fingerprint) {
		t.Fatal("export-sourced fingerprint differs from generator")
	}
}

func TestExternal_NoPubkeySource(t *testing.T) {
	dir := t.TempDir()
	shim := makeShim(t, dir, `echo "export not supported" >&2; exit 3`)
	_, err := LoadExternal(context.Background(), ExternalConfig{
		Command: []string{shim},
	})
	if err == nil {
		t.Fatal("expected error when neither pubkey_file nor export succeed")
	}
	if !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("error %q does not surface child exit code", err)
	}
	if !strings.Contains(err.Error(), "export not supported") {
		t.Errorf("error %q does not surface stderr tail", err)
	}
}

func TestExternal_DetachSignAndClearsign(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "External Test")
	secring := writeSecring(t, e, dir)
	pubPath := writeArmoredPubkey(t, e, dir, "release.pub")

	// Shim delegates detach-sign / clear-sign to the test binary itself,
	// running in SIGN_HELPER_MODE — gives us a real OpenPGP signer without
	// shipping a separate Go binary or pulling in gpg(1). We exercise the
	// wire shape (argv layout, --in/--out, stderr capture).
	helperPath := buildSignHelper(t, dir)

	shim := makeShim(t, dir, `
op="$1"; shift
in=""; out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --in) in="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --key) shift 2 ;;
    --source-date-epoch) shift 2 ;;
    --digest-algo) shift 2 ;;
    *) shift ;;
  esac
done
exec `+helperPath+` "$op" "`+secring+`" "$in" "$out"
`)

	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command:    []string{shim},
		PubkeyFile: pubPath,
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}

	msg := []byte("Origin: Acme\nLabel: Acme APT\nSuite: stable\n")

	t.Run("clearsign", func(t *testing.T) {
		var buf bytes.Buffer
		if err := s.Clearsign(&buf, msg); err != nil {
			t.Fatalf("Clearsign: %v", err)
		}
		if !bytes.HasPrefix(buf.Bytes(), []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
			t.Fatalf("missing clearsign header: %q", buf.Bytes()[:64])
		}
	})

	t.Run("detach-sign", func(t *testing.T) {
		var buf bytes.Buffer
		if err := s.DetachedSign(&buf, msg); err != nil {
			t.Fatalf("DetachedSign: %v", err)
		}
		if !bytes.HasPrefix(buf.Bytes(), []byte("-----BEGIN PGP SIGNATURE-----")) {
			t.Fatalf("missing detached-sig header: %q", buf.Bytes()[:64])
		}
		// Verify against the pubkey embedded in our keyring.
		got, err := openpgp.ReadKeyRing(bytes.NewReader(s.KeyringBytes()))
		if err != nil {
			t.Fatalf("ReadKeyRing: %v", err)
		}
		if _, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList(got), bytes.NewReader(msg), &buf, nil); err != nil {
			t.Fatalf("verify detached sig: %v", err)
		}
	})
}

func TestExternal_NonZeroExitSurfacesStderr(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "Boom")
	pub := writeArmoredPubkey(t, e, dir, "release.pub")

	shim := makeShim(t, dir, `
if [ "$1" = "export" ]; then cat `+pub+`; exit 0; fi
echo "shim refuses to sign" >&2
exit 17
`)
	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command: []string{shim},
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	var buf bytes.Buffer
	err = s.DetachedSign(&buf, []byte("payload"))
	if err == nil {
		t.Fatal("expected error from failing shim")
	}
	if !strings.Contains(err.Error(), "exit 17") {
		t.Errorf("error %q lacks exit code", err)
	}
	if !strings.Contains(err.Error(), "shim refuses to sign") {
		t.Errorf("error %q lacks stderr tail", err)
	}
}

func TestExternal_TimeoutHardKills(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "Slow")
	pub := writeArmoredPubkey(t, e, dir, "release.pub")

	shim := makeShim(t, dir, `
if [ "$1" = "export" ]; then cat `+pub+`; exit 0; fi
sleep 5
`)
	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command: []string{shim},
		Timeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	start := time.Now()
	var buf bytes.Buffer
	err = s.DetachedSign(&buf, []byte("payload"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error %q lacks timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("sign took %s, expected hard kill near 150ms", elapsed)
	}
}

func TestExternal_KeyringBytesStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "Stable")
	pub := writeArmoredPubkey(t, e, dir, "release.pub")
	shim := makeShim(t, dir, `exit 99`)
	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command:    []string{shim},
		PubkeyFile: pub,
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	if !bytes.Equal(s.KeyringBytes(), s.KeyringBytes()) {
		t.Fatal("KeyringBytes diverges across two calls")
	}
}

func TestExternal_NextPubkeyInKeyring(t *testing.T) {
	dir := t.TempDir()
	primary := newTestEntity(t, "Primary")
	next := newTestEntity(t, "Next")

	primaryPath := writeArmoredPubkey(t, primary, dir, "primary.pub")
	nextPath := writeArmoredPubkey(t, next, dir, "next.pub")
	shim := makeShim(t, dir, `exit 99`)

	s, err := LoadExternal(context.Background(), ExternalConfig{
		Command:        []string{shim},
		PubkeyFile:     primaryPath,
		NextPubkeyFile: nextPath,
	})
	if err != nil {
		t.Fatalf("LoadExternal: %v", err)
	}
	got, err := openpgp.ReadKeyRing(bytes.NewReader(s.KeyringBytes()))
	if err != nil {
		t.Fatalf("ReadKeyRing: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d keys, want 2 (primary + next)", len(got))
	}
	if !bytes.Equal(got[0].PrimaryKey.Fingerprint, primary.PrimaryKey.Fingerprint) {
		t.Error("active key should come first")
	}
	if !bytes.Equal(got[1].PrimaryKey.Fingerprint, next.PrimaryKey.Fingerprint) {
		t.Error("next pubkey should come second")
	}
}

func TestExternal_NextPubkeyDuplicateFingerprintRejected(t *testing.T) {
	dir := t.TempDir()
	e := newTestEntity(t, "Dup")
	pub := writeArmoredPubkey(t, e, dir, "release.pub")
	shim := makeShim(t, dir, `exit 99`)

	_, err := LoadExternal(context.Background(), ExternalConfig{
		Command:        []string{shim},
		PubkeyFile:     pub,
		NextPubkeyFile: pub,
	})
	if err == nil {
		t.Fatal("expected error on duplicate fingerprint")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("error did not mention fingerprint: %v", err)
	}
}
