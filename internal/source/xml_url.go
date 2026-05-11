package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/antchfx/xmlquery"
)

// xmlURLBodyCap bounds how much of the metadata response we'll read.
// Vendor XML metadata documents are similar in size to json_url (KiB at
// most for an updates.xml or RSS feed); 1 MiB is plenty of headroom and
// prevents a runaway server from exhausting memory.
const xmlURLBodyCap = 1 << 20

// xmlURLPlaceholder matches `{token}` or `{xpath:...}` substitutions in
// asset_url templates. The literal {token} resolves to the value at
// TokenXPath; any `{xpath:<expr>}` runs <expr> as XPath against the
// metadata body.
//
// We require the `xpath:` prefix for non-token placeholders rather than
// accepting raw XPath in the braces because XPath grammar includes `[`,
// `]`, `/`, predicates with `=`, and other tokens that make a bare
// regex-extracted "any non-brace text" rule brittle. Forcing the prefix
// also keeps the rendered-URL grammar self-documenting.
var xmlURLPlaceholder = regexp.MustCompile(`\{(token|xpath:[^{}]+)\}`)

// LatestXMLDiscoverer fetches an XML metadata endpoint, extracts a
// change-detection token via an XPath expression, and renders a
// templated asset URL using {token} or {xpath:...} placeholders.
//
// This is the XML counterpart to LatestJSONDiscoverer. Targets vendors
// who publish version metadata as XML (JetBrains' updates.xml is the
// canonical case; Apache project release feeds and RSS / Atom feeds
// fit the same shape).
type LatestXMLDiscoverer struct {
	URL        string
	TokenXPath string // XPath resolving to the change-detection token
	AssetURL   string // URL template with {token} or {xpath:...} placeholders

	Client *http.Client
}

// NewLatestXMLDiscoverer constructs a discoverer. Field validation runs
// in the config layer; this constructor only checks for empty inputs as
// a guard against direct callers.
func NewLatestXMLDiscoverer(metadataURL, tokenXPath, assetURL string, client *http.Client) (*LatestXMLDiscoverer, error) {
	if metadataURL == "" || tokenXPath == "" || assetURL == "" {
		return nil, errors.New("xml_url: url, token_xpath, and asset_url are all required")
	}
	return &LatestXMLDiscoverer{
		URL:        metadataURL,
		TokenXPath: tokenXPath,
		AssetURL:   assetURL,
		Client:     client,
	}, nil
}

func (d *LatestXMLDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	client := d.Client
	if client == nil {
		client = in.HTTPClient
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("xml_url: build request: %w", err)
	}
	req.Header.Set("Accept", "application/xml, text/xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xml_url: GET %s: %w", d.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xml_url: GET %s: status %s", d.URL, resp.Status)
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
		return nil, fmt.Errorf("xml_url: parse XML from %s: %w", d.URL, err)
	}

	token, err := xpathFirstString(doc, d.TokenXPath)
	if err != nil {
		return nil, fmt.Errorf("xml_url: token_xpath %q: %w", d.TokenXPath, err)
	}
	if token == "" {
		return nil, fmt.Errorf("xml_url: token_xpath %q resolved to empty string", d.TokenXPath)
	}

	rendered, err := renderXMLAssetURL(d.AssetURL, doc, token)
	if err != nil {
		return nil, fmt.Errorf("xml_url: %w", err)
	}
	if err := assertHTTPURL(rendered); err != nil {
		return nil, fmt.Errorf("xml_url: rendered asset_url: %w", err)
	}

	res := &ProbeResult{Probe: Probe{URL: rendered, Token: token}}
	if in.Prev != nil && in.Prev.Token == token {
		res.Unchanged = true
	}
	return res, nil
}

// renderXMLAssetURL substitutes {token} and {xpath:expr} placeholders in
// template with values pulled from doc. Missing or empty results abort
// with an error rather than producing a malformed URL.
func renderXMLAssetURL(template string, doc *xmlquery.Node, token string) (string, error) {
	var resolveErr error
	out := xmlURLPlaceholder.ReplaceAllStringFunc(template, func(match string) string {
		if resolveErr != nil {
			return match
		}
		inner := match[1 : len(match)-1] // strip { }
		if inner == "token" {
			return token
		}
		expr := strings.TrimPrefix(inner, "xpath:")
		s, err := xpathFirstString(doc, expr)
		if err != nil {
			resolveErr = fmt.Errorf("placeholder {xpath:%s}: %w", expr, err)
			return match
		}
		if s == "" {
			resolveErr = fmt.Errorf("placeholder {xpath:%s} resolved to empty string", expr)
			return match
		}
		return s
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return out, nil
}

// xpathFirstString runs expr against doc and returns the first matching
// node's text value. Returns ("", nil) when the expression compiles but
// matches nothing; that's "empty result" rather than "broken query" and
// callers wrap it into their own context-bearing error.
func xpathFirstString(doc *xmlquery.Node, expr string) (string, error) {
	node, err := xmlquery.Query(doc, expr)
	if err != nil {
		return "", fmt.Errorf("query %q: %w", expr, err)
	}
	if node == nil {
		return "", nil
	}
	return node.InnerText(), nil
}
