package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Downloader streams an HTTP body to disk while computing SHA256 inline.
// Returns the hex digest and the bytes written. The caller compares the
// digest against the plan's expected value (or records a freshly
// computed value when asset.sha256 was nil).
type Downloader struct {
	Client *http.Client
}

// NewDownloader constructs a Downloader using http.DefaultClient when
// none is provided.
func NewDownloader(c *http.Client) *Downloader {
	if c == nil {
		c = http.DefaultClient
	}
	return &Downloader{Client: c}
}

// Fetch GETs url, streams the body into dst, and returns the lowercase
// hex SHA256 plus the byte count. expectedSize, when > 0, is checked
// against bytes received and surfaced as a mismatch error rather than
// reading further. Surfaced errors include redirects and non-2xx status
// codes; the caller can let them propagate as artifact failures.
func (d *Downloader) Fetch(ctx context.Context, url, dst string, expectedSize int64) (sha256Hex string, n int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("new request %s: %w", url, err)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("GET %s: status %s", url, resp.Status)
	}

	out, err := os.Create(dst)
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, h), resp.Body)
	if err != nil {
		return "", written, fmt.Errorf("stream %s: %w", url, err)
	}
	if expectedSize > 0 && written != expectedSize {
		return "", written, fmt.Errorf("size mismatch: got %d bytes, plan declared %d", written, expectedSize)
	}
	if err := out.Sync(); err != nil {
		return "", written, fmt.Errorf("sync %s: %w", dst, err)
	}
	return hex.EncodeToString(h.Sum(nil)), written, nil
}

// VerifySHA256 compares a "sha256:<hex>" plan declaration against a
// freshly computed hex digest. expected may be nil or empty, in which
// case the function returns ok=true unconditionally and a zero-error
// (the caller is responsible for recording the computed value).
func VerifySHA256(expected *string, got string) error {
	if expected == nil || *expected == "" {
		return nil
	}
	const prefix = "sha256:"
	want := *expected
	if !strings.HasPrefix(want, prefix) {
		return fmt.Errorf("plan asset.sha256 %q lacks %q prefix", want, prefix)
	}
	if want[len(prefix):] != got {
		return fmt.Errorf("sha256 mismatch: plan=%s computed=sha256:%s", want, got)
	}
	return nil
}
