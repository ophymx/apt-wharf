// Package keys handles HTTPS fetching, OpenPGP dearmor, and key
// inspection for chandler.
//
// HTTPS is the trust anchor — no fingerprint pinning, no signature
// verification beyond what's needed to dearmor the blob. The inspected
// UID/Fingerprint/Expiry are logged to stderr at discover time and
// recorded as provenance in the emitted plan, but do not gate the
// build. See chandler-design.md "Trust model".
package keys

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Fetched is the result of pulling one keys.<slug> entry to a single
// binary keyring blob: dearmored bytes plus inspection info.
type Fetched struct {
	Binary []byte
	Info   Info
}

// Info is the public-facing summary of one OpenPGP key — what chandler
// logs to stderr at discover time and records in plan provenance.
//
// In the multi-URL case (keys.<slug>.urls fetches several keys that
// chandler concatenates) only the first key's identity is recorded
// here; the provenance record covers the slug as a whole.
type Info struct {
	Fingerprint string // uppercase hex, no spaces
	UID         string // primary user ID, e.g. "Docker Release (CE deb) <docker@docker.com>"
	Expiry      string // RFC 3339 UTC, "" if no expiry
}

// Client is the HTTP client used to fetch key URLs. Nil HTTP uses a
// shared default; tests inject httptest.Server.Client() to avoid network.
type Client struct {
	HTTP *http.Client
}

// MaxBytes caps the response size for one key URL. Real keyrings rarely
// exceed 100 KiB; 1 MiB leaves headroom without inviting abuse.
const MaxBytes = 1 << 20

// defaultHTTPClient is shared across FetchURL calls when the user
// hasn't provided their own. 30s total per request is conservative for
// a one-shot key download.
var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchURL downloads one key URL over HTTPS, dearmors if needed, and
// returns the binary keyring blob plus inspection info.
func (c *Client) FetchURL(ctx context.Context, url string) (*Fetched, error) {
	raw, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	binary, info, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", url, err)
	}
	return &Fetched{Binary: binary, Info: info}, nil
}

// FetchURLs is the multi-URL form: download N keys, dearmor each,
// concatenate the binary blobs. Used when keys.<slug>.urls is set
// (e.g. during vendor key rotation overlap).
//
// The returned Info describes the first URL's key — provenance for the
// slug as a whole records each URL separately at the discover layer.
func (c *Client) FetchURLs(ctx context.Context, urls []string) (*Fetched, error) {
	if len(urls) == 0 {
		return nil, errors.New("FetchURLs: no URLs")
	}
	out := &Fetched{}
	for i, url := range urls {
		f, err := c.FetchURL(ctx, url)
		if err != nil {
			return nil, err
		}
		out.Binary = append(out.Binary, f.Binary...)
		if i == 0 {
			out.Info = f.Info
		}
	}
	return out, nil
}

// Inspect parses a keyring blob and returns the first entity's UID,
// fingerprint, and expiry. Auto-detects armored vs binary; on armored
// input, the dearmored bytes are parsed (the input bytes are not
// modified).
func Inspect(blob []byte) (Info, error) {
	_, info, err := parse(blob)
	return info, err
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	cli := c.HTTP
	if cli == nil {
		cli = defaultHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", url, err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	// LimitReader to MaxBytes+1 so we can detect overruns.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(raw)) > MaxBytes {
		return nil, fmt.Errorf("read %s: response exceeds %d bytes", url, MaxBytes)
	}
	return raw, nil
}

// parse normalizes raw bytes (armored or binary) to a binary keyring
// blob, reads it, and returns the inspection info for the first entity.
func parse(raw []byte) ([]byte, Info, error) {
	bin, err := toBinary(raw)
	if err != nil {
		return nil, Info{}, err
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(bin))
	if err != nil {
		return nil, Info{}, fmt.Errorf("read keyring: %w", err)
	}
	if len(entities) == 0 {
		return nil, Info{}, errors.New("read keyring: no entities found")
	}
	return bin, infoFromEntity(entities[0]), nil
}

// toBinary dearmors raw if armored, returns it as-is if binary. The
// armor sniff is intentionally permissive (any leading whitespace,
// case-sensitive marker) — the marker text is stable across all
// OpenPGP implementations.
func toBinary(raw []byte) ([]byte, error) {
	if !isArmored(raw) {
		return raw, nil
	}
	block, err := armor.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("dearmor: %w", err)
	}
	bin, err := io.ReadAll(block.Body)
	if err != nil {
		return nil, fmt.Errorf("dearmor body: %w", err)
	}
	return bin, nil
}

func isArmored(raw []byte) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("-----BEGIN PGP "))
}

func infoFromEntity(e *openpgp.Entity) Info {
	if e == nil || e.PrimaryKey == nil {
		return Info{}
	}
	return Info{
		Fingerprint: strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)),
		UID:         primaryUID(e),
		Expiry:      primaryExpiry(e),
	}
}

// primaryUID returns the Name of the identity flagged primary by its
// self-signature, falling back to the lexicographically first identity
// for keys that don't mark a primary (single-identity keys, ancient
// keys without the IsPrimaryId subpacket).
func primaryUID(e *openpgp.Entity) string {
	names := sortedIdentityNames(e)
	for _, name := range names {
		id := e.Identities[name]
		if id.SelfSignature != nil && id.SelfSignature.IsPrimaryId != nil && *id.SelfSignature.IsPrimaryId {
			return id.Name
		}
	}
	if len(names) > 0 {
		return e.Identities[names[0]].Name
	}
	return ""
}

// primaryExpiry returns the expiry of the primary key as a RFC 3339
// UTC timestamp, or "" if the key does not expire. KeyLifetimeSecs
// lives on the identity's self-signature in OpenPGP v4; we read it from
// the primary identity (or first one, if no primary marker).
func primaryExpiry(e *openpgp.Entity) string {
	if e.PrimaryKey == nil {
		return ""
	}
	sig := primarySelfSig(e)
	if sig == nil || sig.KeyLifetimeSecs == nil || *sig.KeyLifetimeSecs == 0 {
		return ""
	}
	expiry := e.PrimaryKey.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
	return expiry.UTC().Format(time.RFC3339)
}

func primarySelfSig(e *openpgp.Entity) *packet.Signature {
	names := sortedIdentityNames(e)
	for _, name := range names {
		id := e.Identities[name]
		if id.SelfSignature == nil {
			continue
		}
		if id.SelfSignature.IsPrimaryId != nil && *id.SelfSignature.IsPrimaryId {
			return id.SelfSignature
		}
	}
	for _, name := range names {
		id := e.Identities[name]
		if id.SelfSignature != nil {
			return id.SelfSignature
		}
	}
	return nil
}

func sortedIdentityNames(e *openpgp.Entity) []string {
	if len(e.Identities) == 0 {
		return nil
	}
	names := make([]string, 0, len(e.Identities))
	for k := range e.Identities {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
