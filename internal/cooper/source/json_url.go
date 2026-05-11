package source

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/ophymx/apt-signpost/internal/cooper/config"
)

// jsonURLBodyCap bounds metadata-response size, matching signpost's
// json_url cap. Vendor JSON documents are tiny (KiB at most); 1 MiB is
// plenty of headroom and prevents a runaway server from exhausting
// memory.
const jsonURLBodyCap = 1 << 20

// jsonURLPlaceholder matches `{path}` substitutions in asset_url
// templates. Inner text is either the literal "token" (resolves to the
// extracted version) or a gjson path against the JSON body.
var jsonURLPlaceholder = regexp.MustCompile(`\{([^{}]+)\}`)

// JSONURLResolution is what ResolveJSONURL hands back to the discover
// orchestrator. RawBody is retained so per-arch placeholder rendering
// can re-query gjson paths (cheap; the cap is 1 MiB).
type JSONURLResolution struct {
	URL       string
	Version   string
	RawBody   []byte
	SourceEpoch int64
}

// ResolveJSONURL fetches the configured JSON endpoint, validates the
// payload, extracts the version via VersionPath, and computes a stable
// SourceEpoch (json_url has no published_at equivalent — see
// deriveJSONURLEpoch).
//
// One network call per package, mirroring the github_release path's
// budget. Per-arch URL rendering happens against the returned RawBody
// and doesn't refetch.
func ResolveJSONURL(ctx context.Context, client *http.Client, j *config.JSONURLSource) (*JSONURLResolution, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("json_url: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("json_url: GET %s: %w", j.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("json_url: GET %s: status %s", j.URL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonURLBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("json_url: read body: %w", err)
	}
	if int64(len(body)) > jsonURLBodyCap {
		return nil, fmt.Errorf("json_url: response body exceeds %d bytes", jsonURLBodyCap)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("json_url: response from %s is not valid JSON", j.URL)
	}

	got := gjson.GetBytes(body, j.VersionPath)
	if !got.Exists() {
		return nil, fmt.Errorf("json_url: version_path %q not found in response from %s", j.VersionPath, j.URL)
	}
	version := got.String()
	if version == "" {
		return nil, fmt.Errorf("json_url: version_path %q resolved to empty string", j.VersionPath)
	}
	if j.VersionStripPrefix != "" {
		stripped, ok := strings.CutPrefix(version, j.VersionStripPrefix)
		if !ok {
			return nil, fmt.Errorf("json_url: version_strip_prefix %q not found at start of extracted version %q",
				j.VersionStripPrefix, version)
		}
		if stripped == "" {
			return nil, fmt.Errorf("json_url: stripping %q left an empty version string", j.VersionStripPrefix)
		}
		version = stripped
	}

	return &JSONURLResolution{
		URL:         j.URL,
		Version:     version,
		RawBody:     body,
		SourceEpoch: deriveJSONURLEpoch(j.URL, version),
	}, nil
}

// RenderAssetURL substitutes ${VERSION} / ${ARCH} (cooper's standard
// substitutions) and {token} / {gjson.path} (signpost-compatible)
// placeholders in template, returning the final URL. Errors on missing
// placeholders rather than emitting a malformed URL.
func RenderAssetURL(template, version, arch string, body []byte) (string, error) {
	// Cooper's ${IDENT} substitutions first.
	expanded := strings.ReplaceAll(template, "${VERSION}", version)
	expanded = strings.ReplaceAll(expanded, "${ARCH}", arch)

	// signpost-style {token}/{gjson.path}.
	var resolveErr error
	out := jsonURLPlaceholder.ReplaceAllStringFunc(expanded, func(match string) string {
		if resolveErr != nil {
			return match
		}
		path := match[1 : len(match)-1]
		if path == "token" {
			return version
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
	if err := assertHTTPURL(out); err != nil {
		return "", fmt.Errorf("rendered asset_url: %w", err)
	}
	return out, nil
}

// deriveJSONURLEpoch produces a stable timestamp for json_url sources.
// github_release uses release.published_at; json_url has no canonical
// equivalent, so we hash (url, version) into a deterministic second
// offset from a fixed 2020-01-01 base, capped to a ~10-year window.
//
// The exact value is meaningless — reproducibility just needs the same
// plan to yield the same epoch, and a "modern-ish" timestamp keeps
// `ar tv` listings readable. Same version → same bytes → same epoch.
func deriveJSONURLEpoch(metadataURL, version string) int64 {
	const (
		baseEpoch int64 = 1577836800           // 2020-01-01T00:00:00Z
		spanSecs  int64 = 10 * 365 * 24 * 3600 // ~10 years
	)
	h := sha256.Sum256([]byte(metadataURL + ":" + version))
	n := int64(binary.BigEndian.Uint64(h[:8]) >> 1) // clamp to positive int64
	return baseEpoch + (n % spanSecs)
}

// assertHTTPURL guards against rendered URLs whose scheme isn't
// http(s) — otherwise a malicious or malformed JSON value could
// persuade us to hand the fetcher a file:// or other scheme.
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
