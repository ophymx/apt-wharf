package source

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// LatestURLDiscoverer probes an upstream "latest" URL: an HTTP endpoint that
// either serves the current .deb directly or redirects to the current
// versioned download URL. Token preference order, per design.md, is:
// final resolved URL → ETag → Last-Modified → empty (always-fetch).
//
// When the HEAD resolves to an S3 bucket, the discoverer sends
// x-amz-checksum-mode: ENABLED and surfaces any returned
// x-amz-checksum-sha256 as Probe.AssetDigest so the refresher can skip the
// streaming hash. Go's redirect machinery forwards custom headers across
// hops other than Authorization/Cookie, so a single HEAD covers the
// "redirector → S3" case.
type LatestURLDiscoverer struct {
	URL string

	// Client is used for the HEAD probe. When nil, falls back to
	// ProbeInput.HTTPClient and finally http.DefaultClient.
	Client *http.Client

	// isS3 reports whether the response's resolved URL is on AWS S3.
	// Overridable for tests since httptest URLs never look like S3.
	isS3 func(*url.URL) bool
}

// NewLatestURLDiscoverer constructs a discoverer. URL validation happened in
// the config layer; this constructor only checks non-emptiness as a guard
// against direct callers.
func NewLatestURLDiscoverer(latestURL string, client *http.Client) (*LatestURLDiscoverer, error) {
	if latestURL == "" {
		return nil, errors.New("latest_url: url is required")
	}
	return &LatestURLDiscoverer{URL: latestURL, Client: client, isS3: defaultIsS3Host}, nil
}

func (d *LatestURLDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	client := d.Client
	if client == nil {
		client = in.HTTPClient
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("latest_url: build request: %w", err)
	}
	// Harmless on non-S3 servers; preserved across cross-domain redirects by
	// net/http (it only strips Authorization/Cookie/etc).
	req.Header.Set("x-amz-checksum-mode", "ENABLED")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("latest_url: HEAD %s: %w", d.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("latest_url: HEAD %s: status %s", d.URL, resp.Status)
	}

	finalURL := resp.Request.URL.String()
	token := finalURL
	if finalURL == d.URL {
		// No redirect occurred; the URL itself is no freshness signal, so
		// fall back to validators in the response headers.
		if et := strings.TrimSpace(resp.Header.Get("ETag")); et != "" {
			token = et
		} else if lm := strings.TrimSpace(resp.Header.Get("Last-Modified")); lm != "" {
			token = lm
		} else {
			token = ""
		}
	}

	digest := ""
	if d.isS3 != nil && d.isS3(resp.Request.URL) {
		if cs := resp.Header.Get("x-amz-checksum-sha256"); cs != "" {
			if hx, ok := decodeS3SHA256(cs); ok {
				digest = hx
			}
		}
	}

	res := &ProbeResult{Probe: Probe{URL: finalURL, Token: token, AssetDigest: digest}}
	if in.Prev != nil && token != "" && in.Prev.Token == token {
		res.Unchanged = true
	}
	return res, nil
}

// defaultIsS3Host returns true for AWS S3 endpoint shapes:
// {bucket}.s3.amazonaws.com, {bucket}.s3.{region}.amazonaws.com,
// s3.amazonaws.com/{bucket}/..., s3.{region}.amazonaws.com/{bucket}/....
// CNAME'd vanity domains pointing at S3 are not detected; that's an
// acceptable miss because we only opt in to using the header, never out.
func defaultIsS3Host(u *url.URL) bool {
	if u == nil {
		return false
	}
	h := u.Hostname()
	if !strings.HasSuffix(h, ".amazonaws.com") {
		return false
	}
	// Match the s3 label as a token, not a substring of an unrelated host
	// (e.g., something-s3y.amazonaws.com would NOT match).
	for _, label := range strings.Split(h, ".") {
		if label == "s3" {
			return true
		}
		if strings.HasPrefix(label, "s3-") || strings.HasPrefix(label, "s3.") {
			return true
		}
	}
	return false
}

// decodeS3SHA256 turns S3's base64-encoded sha256 into a lowercase hex string.
// Returns ok=false on any decode error or wrong length, so callers fall back
// to streaming-hash rather than trusting a malformed value.
func decodeS3SHA256(b64 string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != 32 {
		return "", false
	}
	return hex.EncodeToString(raw), true
}
