package source

import (
	"context"
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
	URL         string
	Version     string
	RawBody     []byte
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
		SourceEpoch: deriveURLEpoch(j.URL, version),
	}, nil
}

// RenderAssetURLs is the plural counterpart to RenderAssetURL. Renders
// each template in order; any failure aborts the whole batch so the
// artifact lands as result=error rather than half-resolved. Same
// no-collision invariant as MatchAssets: rendered URL basenames must
// be unique because all assets land under one ${ASSETS}/ directory.
func RenderAssetURLs(templates []string, version, arch string, body []byte) ([]string, error) {
	out := make([]string, 0, len(templates))
	seen := make(map[string]int, len(templates))
	for i, t := range templates {
		u, err := RenderAssetURL(t, version, arch, body)
		if err != nil {
			return nil, fmt.Errorf("asset_urls[%d]: %w", i, err)
		}
		name := basenameOf(u)
		if prev, ok := seen[name]; ok {
			return nil, fmt.Errorf("asset name collision under ${ASSETS}/: asset_urls[%d]=%q and asset_urls[%d]=%q both resolved to basename %s",
				prev, templates[prev], i, t, name)
		}
		seen[name] = i
		out = append(out, u)
	}
	return out, nil
}

// basenameOf is the discover-side equivalent of build's basename
// extractor — strips query/fragment and returns the URL's last path
// segment. Used for the discover-time collision check; build's own
// basenameFromURL stays the source of truth for asset.name.
func basenameOf(rawURL string) string {
	end := len(rawURL)
	for i := 0; i < len(rawURL); i++ {
		if rawURL[i] == '?' || rawURL[i] == '#' {
			end = i
			break
		}
	}
	s := rawURL[:end]
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
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
