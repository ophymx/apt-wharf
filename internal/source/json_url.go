package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"

	"github.com/tidwall/gjson"
)

// jsonURLBodyCap bounds how much of the metadata response we'll read.
// Vendor JSON metadata documents are tiny (KiB at most); 1 MiB is plenty
// of headroom and prevents a runaway server from exhausting memory.
const jsonURLBodyCap = 1 << 20

// jsonURLPlaceholder matches `{path}` substitutions in asset_url templates.
// Inner text is a gjson path (e.g. result.downloadVO.zoom.version) or the
// literal "token", which resolves to the value at TokenPath.
var jsonURLPlaceholder = regexp.MustCompile(`\{([^{}]+)\}`)

// LatestJSONDiscoverer fetches a JSON metadata endpoint, extracts a
// change-detection token via a gjson path, and renders a templated asset
// URL using either {token} or arbitrary {gjson.path} placeholders.
//
// The pattern absorbs vendor download endpoints that publish JSON
// metadata (Discord, Slack, Zoom, Cypress, Postman) — each used to require
// an `external` shim binary; with json_url they're config-only.
type LatestJSONDiscoverer struct {
	URL       string
	TokenPath string // gjson path resolving to the change-detection token
	AssetURL  string // URL template with {token} or {gjson.path} placeholders

	Client *http.Client
}

// NewLatestJSONDiscoverer constructs a discoverer. Field validation runs in
// the config layer; this constructor only checks for empty inputs as a
// guard against direct callers.
func NewLatestJSONDiscoverer(metadataURL, tokenPath, assetURL string, client *http.Client) (*LatestJSONDiscoverer, error) {
	if metadataURL == "" || tokenPath == "" || assetURL == "" {
		return nil, errors.New("json_url: url, token_path, and asset_url are all required")
	}
	return &LatestJSONDiscoverer{
		URL:       metadataURL,
		TokenPath: tokenPath,
		AssetURL:  assetURL,
		Client:    client,
	}, nil
}

func (d *LatestJSONDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	client := d.Client
	if client == nil {
		client = in.HTTPClient
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("json_url: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("json_url: GET %s: %w", d.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("json_url: GET %s: status %s", d.URL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonURLBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("json_url: read body: %w", err)
	}
	if int64(len(body)) > jsonURLBodyCap {
		return nil, fmt.Errorf("json_url: response body exceeds %d bytes", jsonURLBodyCap)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("json_url: response from %s is not valid JSON", d.URL)
	}

	tokenResult := gjson.GetBytes(body, d.TokenPath)
	if !tokenResult.Exists() {
		return nil, fmt.Errorf("json_url: token_path %q not found in response", d.TokenPath)
	}
	token := tokenResult.String()
	if token == "" {
		return nil, fmt.Errorf("json_url: token_path %q resolved to empty string", d.TokenPath)
	}

	rendered, err := renderJSONAssetURL(d.AssetURL, body, token)
	if err != nil {
		return nil, fmt.Errorf("json_url: %w", err)
	}
	if err := assertHTTPURL(rendered); err != nil {
		return nil, fmt.Errorf("json_url: rendered asset_url: %w", err)
	}

	res := &ProbeResult{Probe: Probe{URL: rendered, Token: token}}
	if in.Prev != nil && in.Prev.Token == token {
		res.Unchanged = true
	}
	return res, nil
}

// renderJSONAssetURL substitutes {placeholder}s in template with values
// pulled from body. The literal {token} resolves to the value at TokenPath
// (already extracted by the caller); any other placeholder is treated as a
// gjson path against body. Missing placeholders abort with an error rather
// than producing a malformed URL.
func renderJSONAssetURL(template string, body []byte, token string) (string, error) {
	var resolveErr error
	out := jsonURLPlaceholder.ReplaceAllStringFunc(template, func(match string) string {
		if resolveErr != nil {
			return match
		}
		path := match[1 : len(match)-1]
		if path == "token" {
			return token
		}
		v := gjson.GetBytes(body, path)
		if !v.Exists() {
			resolveErr = fmt.Errorf("placeholder {%s} not found in response", path)
			return match
		}
		s := v.String()
		if s == "" {
			resolveErr = fmt.Errorf("placeholder {%s} resolved to empty string", path)
			return match
		}
		return s
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return out, nil
}

// assertHTTPURL guards against rendered URLs whose scheme isn't http(s) —
// otherwise a malicious or malformed JSON value could persuade us to hand
// the fetcher a file:// or other scheme that misbehaves.
func assertHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be http or https (got %q)", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}
