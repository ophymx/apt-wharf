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
	"context"
	"errors"
	"net/http"
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
	UID         string // primary user ID string, e.g. "Docker Release (CE deb) <docker@docker.com>"
	Expiry      string // RFC 3339 UTC, "" if no expiry
}

// Client is the HTTP client used to fetch key URLs. Nil means use a
// reasonable default; tests inject their own client to avoid network.
type Client struct {
	HTTP *http.Client
}

// FetchURL downloads one key URL over HTTPS, dearmors if needed, and
// returns the binary keyring blob plus inspection info.
//
// TODO: implement HTTPS GET, response-size cap, dearmor via
// github.com/ProtonMail/go-crypto/openpgp.
func (c *Client) FetchURL(ctx context.Context, url string) (*Fetched, error) {
	return nil, errors.New("keys.FetchURL: not yet implemented")
}

// FetchURLs is the multi-URL form: download N keys, dearmor each,
// concatenate the binary blobs. Used when keys.<slug>.urls is set
// (e.g. during vendor key rotation overlap).
//
// TODO: implement.
func (c *Client) FetchURLs(ctx context.Context, urls []string) (*Fetched, error) {
	return nil, errors.New("keys.FetchURLs: not yet implemented")
}

// Inspect parses an already-binary keyring blob and returns its UID,
// fingerprint, and expiry. Used to validate locally-staged keyrings
// (a sibling of FetchURL that skips the network).
//
// TODO: implement via openpgp.ReadKeyRing.
func Inspect(binary []byte) (Info, error) {
	return Info{}, errors.New("keys.Inspect: not yet implemented")
}
