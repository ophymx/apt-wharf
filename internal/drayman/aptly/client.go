// Package aptly is a thin HTTP client for aptly's REST API. drayman
// uses it to query a published apt repository for already-imported
// packages (by X-Cooper-Build-Inputs-Hash or by name+arch), upload
// .deb files, and trigger publish updates.
//
// See https://www.aptly.info/doc/api/swagger/ for the upstream API
// reference. drayman touches a small subset: /api/repos/{name}/packages
// (GET, plus POST/DELETE for write paths), /api/files/{dir} (POST
// upload), /api/repos/{name}/file/{dir} (POST import), and
// /api/publish/{prefix}/{distribution} (PUT update). Write paths are
// in a follow-up commit.
package aptly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to a single aptly API instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New constructs a Client. baseURL is the aptly API base (typically
// "http://localhost:8080" with no trailing slash; New strips any
// trailing slash). hc may be nil; New substitutes http.DefaultClient.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    hc,
	}
}

// Package mirrors the per-record shape aptly returns under
// ?format=details. drayman only reads a handful of fields — Package,
// Version, Architecture, Key, X-Cooper-Build-Inputs-Hash — but the
// struct preserves the standard set so future callers can use more
// without redefining the wire format.
type Package struct {
	Architecture            string `json:"Architecture"`
	XCooperBuildInputsHash  string `json:"X-Cooper-Build-Inputs-Hash,omitempty"`
	Description             string `json:"Description,omitempty"`
	Filename                string `json:"Filename,omitempty"`
	FilesHash               string `json:"FilesHash,omitempty"`
	Key                     string `json:"Key"`
	MD5sum                  string `json:"MD5sum,omitempty"`
	Maintainer              string `json:"Maintainer,omitempty"`
	Package                 string `json:"Package"`
	SHA256                  string `json:"SHA256,omitempty"`
	Section                 string `json:"Section,omitempty"`
	ShortKey                string `json:"ShortKey,omitempty"`
	Size                    string `json:"Size,omitempty"`
	Version                 string `json:"Version"`
}

// QueryPackages returns packages in repo matching the aptly package
// query DSL string q. An empty q lists every package in the repo.
//
// The query DSL is documented at
// https://www.aptly.info/doc/feature/query/. drayman uses two shapes:
//
//   - "X-Cooper-Build-Inputs-Hash (= sha256:<hex>)" — exact hash match
//   - "<name> {<arch>}"                              — by name and arch
//
// Any non-2xx response is reported with the URL and response body for
// debuggability.
func (c *Client) QueryPackages(ctx context.Context, repo, q string) ([]Package, error) {
	u := fmt.Sprintf("%s/api/repos/%s/packages", c.baseURL, url.PathEscape(repo))
	qv := url.Values{}
	qv.Set("format", "details")
	if q != "" {
		qv.Set("q", q)
	}
	u += "?" + qv.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("aptly GET %s: %d %s", req.URL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []Package
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("aptly decode: %w", err)
	}
	return out, nil
}

// HashExists reports whether any package in repo carries the given
// X-Cooper-Build-Inputs-Hash. hash should be the full "sha256:<hex>"
// form (no surrounding quotes).
//
// This is drayman's primary dedup query — see cooper-design.md
// §"Primary dedup key: X-Cooper-Build-Inputs-Hash".
func (c *Client) HashExists(ctx context.Context, repo, hash string) (bool, error) {
	pkgs, err := c.QueryPackages(ctx, repo, fmt.Sprintf("X-Cooper-Build-Inputs-Hash (= %s)", hash))
	if err != nil {
		return false, err
	}
	return len(pkgs) > 0, nil
}

// ListByNameArch returns every package in repo with the given Debian
// package Name and Architecture. drayman uses this to enumerate the
// versions of (Package, Architecture) for max-revision computation
// during auto-bump.
func (c *Client) ListByNameArch(ctx context.Context, repo, name, arch string) ([]Package, error) {
	return c.QueryPackages(ctx, repo, fmt.Sprintf("%s {%s}", name, arch))
}
