package sign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ophymx/apt-wharf/internal/source"
)

// ExternalConfig describes the exec contract that backs externalSigner.
//
// Per signing op, signpost runs:
//
//	<Command...> <op> --key <Key>
//	    --in <staged-input> --out <staged-output>
//	    [--source-date-epoch <unix>]
//	    [--digest-algo SHA256]
//
// where <op> is "detach-sign" or "clear-sign". --key and --digest-algo are
// omitted when their config values are empty. Output is whatever the tool
// writes to the --out path; signpost reads that file and feeds it into the
// snapshot. Stdin is unused; stderr is captured and surfaced in errors
// (last 1 KiB) and as INFO log lines.
//
// The shape matches `vault-pgp-sign(1)` 1:1 so that binary can be wired
// directly; everything else fits behind a thin shell shim. See
// packaging/contrib/vault-pgp-sign-shim.sh for the reference shim and
// design.md "GPG / signing" for the rationale.
type ExternalConfig struct {
	// Command is the program plus any leading argv supplied verbatim before
	// the signpost-added subcommand. Command[0] must be an absolute path —
	// PATH is not inherited.
	Command []string

	// Key, when non-empty, is forwarded as `--key <Key>` to every op. Tools
	// with a single bound key (e.g. shim scripts that hard-wire it) leave
	// this empty.
	Key string

	// Timeout caps a single op. 0 selects the package default.
	Timeout time.Duration

	// Env is the per-signer environment passed to the child on top of the
	// proxy allowlist (HTTP_PROXY/HTTPS_PROXY/NO_PROXY and lowercase peers).
	// Anything else from signpost's environment is dropped. Mirrors the
	// external-discovery contract — see internal/source/external.go.
	Env map[string]string

	// PubkeyFile, when non-empty, is read at startup to seed KeyringBytes.
	// Accepts armored or binary OpenPGP, exactly one key. When empty,
	// LoadExternal falls back to invoking the export op (see below).
	PubkeyFile string

	// NextPubkeyFile, when non-empty, is the rotation soak-period second
	// pubkey shipped in the bootstrap keyring. Same semantics as the
	// internal signer's next_pubkey_file.
	NextPubkeyFile string

	// Logger receives child stderr at DEBUG on success / INFO on error,
	// tagged with op and key. Nil silences capture.
	Logger *slog.Logger
}

// defaultExternalTimeout applies when ExternalConfig.Timeout is 0.
const defaultExternalSignTimeout = 30 * time.Second

// envAllowlist mirrors internal/source/external.go — proxy vars are
// useful and otherwise inert; everything else gets dropped so the
// child sandbox stays predictable.
var envAllowlist = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// LoadExternal builds an external Signer. The pubkey is resolved at
// startup so KeyringBytes() can return cached bytes and the bootstrap
// input hash stays stable across reload.
//
// Pubkey resolution order:
//
//  1. cfg.PubkeyFile, when set.
//  2. `<Command> export --key <Key>` against the configured command.
//
// One of the two must succeed; the error joins both attempts so the
// operator sees what each branch tried.
func LoadExternal(ctx context.Context, cfg ExternalConfig) (Signer, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("signing.external.command must list at least one entry")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultExternalSignTimeout
	}

	primary, pubkeySource, err := resolveExternalPubkey(ctx, cfg)
	if err != nil {
		return nil, err
	}

	s := &externalSigner{
		cfg:     cfg,
		primary: primary,
	}

	if cfg.NextPubkeyFile != "" {
		raw, err := os.ReadFile(cfg.NextPubkeyFile)
		if err != nil {
			return nil, fmt.Errorf("signing.external.next_pubkey_file %s: %w", cfg.NextPubkeyFile, err)
		}
		next, err := parseEntity(raw)
		if err != nil {
			return nil, fmt.Errorf("signing.external.next_pubkey_file %s: %w", cfg.NextPubkeyFile, err)
		}
		if bytes.Equal(primary.PrimaryKey.Fingerprint, next.PrimaryKey.Fingerprint) {
			return nil, fmt.Errorf(
				"signing.external.next_pubkey_file %s has the same fingerprint as the active key — rotation requires distinct keys",
				cfg.NextPubkeyFile)
		}
		s.nextPub = next
	}

	s.keyring = canonicalKeyring(primary, s.nextPub)

	if cfg.Logger != nil {
		cfg.Logger.Info("external signer ready",
			"command", cfg.Command[0],
			"key", cfg.Key,
			"pubkey_source", pubkeySource,
			"fingerprint", fmt.Sprintf("%X", primary.PrimaryKey.Fingerprint))
	}

	return s, nil
}

// resolveExternalPubkey returns the parsed primary entity plus a short
// human-readable tag describing where it came from.
func resolveExternalPubkey(ctx context.Context, cfg ExternalConfig) (*openpgp.Entity, string, error) {
	if cfg.PubkeyFile != "" {
		raw, err := os.ReadFile(cfg.PubkeyFile)
		if err != nil {
			return nil, "", fmt.Errorf("signing.external.pubkey_file %s: %w", cfg.PubkeyFile, err)
		}
		e, err := parseEntity(raw)
		if err != nil {
			return nil, "", fmt.Errorf("signing.external.pubkey_file %s: %w", cfg.PubkeyFile, err)
		}
		return e, "pubkey_file " + cfg.PubkeyFile, nil
	}

	// No pubkey_file — invoke `export`. Captures stdout (cert bytes,
	// armored or binary) and parses it. Stderr rides any error.
	out, stderr, err := runExternal(ctx, cfg, "export", nil, nil)
	if err != nil {
		return nil, "", fmt.Errorf(
			"signing.external.pubkey_file not set and `%s export` failed: %w%s",
			cfg.Command[0], err, stderrTail(stderr))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, "", fmt.Errorf(
			"signing.external.pubkey_file not set and `%s export` produced empty output",
			cfg.Command[0])
	}
	e, err := parseEntity(out)
	if err != nil {
		return nil, "", fmt.Errorf(
			"signing.external `%s export` output: %w", cfg.Command[0], err)
	}
	return e, "export subcommand", nil
}

// canonicalKeyring serializes primary (and next, when present) to binary
// OpenPGP in canonical "active then next" order. Stable across calls —
// the bootstrap input hash depends on byte equality across ticks.
func canonicalKeyring(primary, next *openpgp.Entity) []byte {
	var buf bytes.Buffer
	_ = primary.Serialize(&buf)
	if next != nil {
		_ = next.Serialize(&buf)
	}
	return buf.Bytes()
}

// externalSigner implements Signer by exec'ing the configured CLI per op.
type externalSigner struct {
	cfg     ExternalConfig
	primary *openpgp.Entity
	nextPub *openpgp.Entity
	keyring []byte
}

func (s *externalSigner) Clearsign(w io.Writer, message []byte) error {
	return s.runSignOp(context.Background(), "clear-sign", w, message)
}

func (s *externalSigner) DetachedSign(w io.Writer, message []byte) error {
	return s.runSignOp(context.Background(), "detach-sign", w, message)
}

func (s *externalSigner) KeyringBytes() []byte { return s.keyring }

// Zero is a no-op for the external signer. No secret material lives in
// this process.
func (s *externalSigner) Zero() {}

// runSignOp stages message to a tmp file, exec's the configured CLI with
// --in/--out and (when set) --key + --source-date-epoch, then reads the
// signed output back. Both files live under a freshly-created 0700 tmp
// dir that's removed on exit — defense in depth against a misbehaved
// child writing somewhere unexpected.
func (s *externalSigner) runSignOp(ctx context.Context, op string, w io.Writer, message []byte) error {
	dir, err := os.MkdirTemp("", "signpost-external-sign-*")
	if err != nil {
		return fmt.Errorf("external %s: tempdir: %w", op, err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in")
	outPath := filepath.Join(dir, "out")
	if err := os.WriteFile(inPath, message, 0o600); err != nil {
		return fmt.Errorf("external %s: stage input: %w", op, err)
	}

	extra := []string{"--in", inPath, "--out", outPath}
	// Pass SOURCE_DATE_EPOCH so signing backends with reproducibility
	// hooks (vault-pgp-sign --source-date-epoch, gpg --faked-system-time)
	// can pin signature creation time. Sourced from env, falling back to
	// the build/refresh tick's natural moment.
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		// Surface obviously-bad values early instead of silently dropping.
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			extra = append(extra, "--source-date-epoch", v)
		}
	}

	if _, stderr, err := runExternal(ctx, s.cfg, op, extra, message); err != nil {
		return fmt.Errorf("external %s: %w%s", op, err, stderrTail(stderr))
	}

	signed, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("external %s: read output: %w", op, err)
	}
	if len(signed) == 0 {
		return fmt.Errorf("external %s: wrote empty output to %s", op, outPath)
	}
	if _, err := w.Write(signed); err != nil {
		return fmt.Errorf("external %s: write: %w", op, err)
	}
	return nil
}

// runExternal is the common exec wrapper used by both pubkey export and
// the signing ops. op is forwarded as the first non-Command argv. extras
// follow; --key is inserted between when configured.
//
// Returns (stdout, stderr, error). stderr is always populated when the
// child emitted any; the caller decides how to surface it.
func runExternal(ctx context.Context, cfg ExternalConfig, op string, extras []string, stdin []byte) ([]byte, []byte, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultExternalSignTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := make([]string, 0, len(cfg.Command)+4+len(extras))
	argv = append(argv, cfg.Command[1:]...)
	argv = append(argv, op)
	if cfg.Key != "" {
		argv = append(argv, "--key", cfg.Key)
	}
	argv = append(argv, extras...)

	cmd := exec.CommandContext(cctx, cfg.Command[0], argv...)
	cmd.Env = buildEnv(cfg.Env)
	source.SetProcAttrs(cmd)
	cmd.WaitDelay = 2 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	logStderr(cfg.Logger, op, cfg.Key, stderrBuf.Bytes(), runErr != nil)

	if runErr != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return stdoutBuf.Bytes(), stderrBuf.Bytes(),
				fmt.Errorf("timeout after %s", timeout)
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return stdoutBuf.Bytes(), stderrBuf.Bytes(),
				fmt.Errorf("exit %d", ee.ExitCode())
		}
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), runErr
	}
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), nil
}

func buildEnv(extra map[string]string) []string {
	env := make([]string, 0, len(envAllowlist)+len(extra))
	for _, name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func logStderr(logger *slog.Logger, op, key string, b []byte, isError bool) {
	if logger == nil || len(bytes.TrimSpace(b)) == 0 {
		return
	}
	level := slog.LevelDebug
	if isError {
		level = slog.LevelInfo
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		logger.Log(context.Background(), level, "external signer stderr",
			"op", op, "key", key, "line", line)
	}
}

const stderrSnippetMax = 1024

// stderrTail returns up to the last 1 KiB of stderr formatted for inclusion
// in an error message (": <text>"), or the empty string if stderr is empty.
func stderrTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > stderrSnippetMax {
		s = s[len(s)-stderrSnippetMax:]
	}
	return ": " + s
}
