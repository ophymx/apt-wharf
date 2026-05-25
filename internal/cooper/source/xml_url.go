package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/antchfx/xmlquery"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
)

// xmlURLBodyCap bounds metadata-response size, matching json_url.
// Vendor XML metadata documents (updates.xml, RSS / Atom feeds) are
// KiB at most; 1 MiB is plenty of headroom and prevents a runaway
// server from exhausting memory.
const xmlURLBodyCap = 1 << 20

// xmlURLPlaceholder matches `{token}` or `{xpath:expr}` substitutions
// in asset_url templates. The `xpath:` prefix exists because XPath
// expressions can contain `[ ] / =` etc. — accepting raw XPath inside
// braces would clash with the surrounding URL grammar. Forcing the
// prefix also keeps the rendered-URL grammar self-documenting and
// parallels signpost's xml_url discoverer.
var xmlURLPlaceholder = regexp.MustCompile(`\{(token|xpath:[^{}]+)\}`)

// XMLURLResolution is the xml_url counterpart to JSONURLResolution.
// RawBody is retained so per-arch placeholder rendering can re-query
// XPath expressions against the already-fetched body.
type XMLURLResolution struct {
	URL         string
	Version     string
	RawBody     []byte
	SourceEpoch int64
}

// ResolveXMLURL fetches the configured XML endpoint, validates the
// payload parses as XML, extracts the version via VersionXPath, and
// computes a stable SourceEpoch the same way ResolveJSONURL does
// (deriveURLEpoch).
//
// One network call per package, mirroring json_url's budget. Per-arch
// URL rendering happens against the returned RawBody and doesn't
// refetch.
func ResolveXMLURL(ctx context.Context, client *http.Client, x *config.XMLURLSource) (*XMLURLResolution, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("xml_url: build request: %w", err)
	}
	req.Header.Set("Accept", "application/xml, text/xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xml_url: GET %s: %w", x.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xml_url: GET %s: status %s", x.URL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, xmlURLBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("xml_url: read body: %w", err)
	}
	if int64(len(body)) > xmlURLBodyCap {
		return nil, fmt.Errorf("xml_url: response body exceeds %d bytes", xmlURLBodyCap)
	}

	doc, err := xmlquery.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("xml_url: parse XML from %s: %w", x.URL, err)
	}

	node, err := xmlquery.Query(doc, x.VersionXPath)
	if err != nil {
		return nil, fmt.Errorf("xml_url: version_xpath %q: %w", x.VersionXPath, err)
	}
	if node == nil {
		return nil, fmt.Errorf("xml_url: version_xpath %q not found in response from %s", x.VersionXPath, x.URL)
	}
	version := node.InnerText()
	if version == "" {
		return nil, fmt.Errorf("xml_url: version_xpath %q resolved to empty string", x.VersionXPath)
	}
	if x.VersionStripPrefix != "" {
		stripped, ok := strings.CutPrefix(version, x.VersionStripPrefix)
		if !ok {
			return nil, fmt.Errorf("xml_url: version_strip_prefix %q not found at start of extracted version %q",
				x.VersionStripPrefix, version)
		}
		if stripped == "" {
			return nil, fmt.Errorf("xml_url: stripping %q left an empty version string", x.VersionStripPrefix)
		}
		version = stripped
	}

	return &XMLURLResolution{
		URL:         x.URL,
		Version:     version,
		RawBody:     body,
		SourceEpoch: deriveURLEpoch(x.URL, version),
	}, nil
}

// RenderXMLAssetURLs is the plural counterpart to RenderXMLAssetURL.
// Renders each template against the same XML body; collision check
// matches the json_url path (basenames under ${ASSETS}/ must be
// distinct).
func RenderXMLAssetURLs(templates []string, version, arch string, body []byte) ([]string, error) {
	out := make([]string, 0, len(templates))
	seen := make(map[string]int, len(templates))
	for i, t := range templates {
		u, err := RenderXMLAssetURL(t, version, arch, body)
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

// RenderXMLAssetURL substitutes ${VERSION} / ${ARCH} (cooper's standard
// substitutions) and {token} / {xpath:expr} (signpost-compatible)
// placeholders in template, returning the final URL. Errors on missing
// placeholders rather than emitting a malformed URL.
//
// The function re-parses body on every call; vendor XML documents are
// small (KiB) and the typical caller renders one URL per arch (2-3
// times per package), so the cost is negligible compared with the
// network round-trip.
func RenderXMLAssetURL(template, version, arch string, body []byte) (string, error) {
	// Cooper's ${IDENT} substitutions first.
	expanded := strings.ReplaceAll(template, "${VERSION}", version)
	expanded = strings.ReplaceAll(expanded, "${ARCH}", arch)

	// signpost-style {token}/{xpath:...}.
	doc, err := xmlquery.Parse(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("xml_url: re-parse body for placeholder rendering: %w", err)
	}
	var resolveErr error
	out := xmlURLPlaceholder.ReplaceAllStringFunc(expanded, func(match string) string {
		if resolveErr != nil {
			return match
		}
		inner := match[1 : len(match)-1]
		if inner == "token" {
			return version
		}
		expr := strings.TrimPrefix(inner, "xpath:")
		node, qerr := xmlquery.Query(doc, expr)
		if qerr != nil {
			resolveErr = fmt.Errorf("placeholder {xpath:%s}: %w", expr, qerr)
			return match
		}
		if node == nil {
			resolveErr = fmt.Errorf("placeholder {xpath:%s} not found in response", expr)
			return match
		}
		s := node.InnerText()
		if s == "" {
			resolveErr = fmt.Errorf("placeholder {xpath:%s} resolved to empty string", expr)
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
