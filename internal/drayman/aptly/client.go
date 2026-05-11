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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	Architecture           string `json:"Architecture"`
	XCooperBuildInputsHash string `json:"X-Cooper-Build-Inputs-Hash,omitempty"`
	Description            string `json:"Description,omitempty"`
	Filename               string `json:"Filename,omitempty"`
	FilesHash              string `json:"FilesHash,omitempty"`
	Key                    string `json:"Key"`
	MD5sum                 string `json:"MD5sum,omitempty"`
	Maintainer             string `json:"Maintainer,omitempty"`
	Package                string `json:"Package"`
	SHA256                 string `json:"SHA256,omitempty"`
	Section                string `json:"Section,omitempty"`
	ShortKey               string `json:"ShortKey,omitempty"`
	Size                   string `json:"Size,omitempty"`
	Version                string `json:"Version"`
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

// encodeAptlyPrefix applies aptly's URL convention for publication
// prefixes (see aptly docs §"REST API: GET /api/publish"):
//
//   - "." (no prefix) becomes ":." — a bare "." would be collapsed
//     by HTTP intermediaries.
//   - "_" becomes "__" (must double first).
//   - "/" becomes "_" (so hierarchical prefixes like "ubuntu/jammy"
//     reach aptly as "ubuntu_jammy").
//
// Escape ordering matters: literal "_" must double before "/"
// collapses, otherwise the round-trip is ambiguous.
func encodeAptlyPrefix(prefix string) string {
	if prefix == "." {
		return ":."
	}
	prefix = strings.ReplaceAll(prefix, "_", "__")
	prefix = strings.ReplaceAll(prefix, "/", "_")
	return prefix
}

// ListByNameArch returns every package in repo with the given Debian
// package Name and Architecture. drayman uses this to enumerate the
// versions of (Package, Architecture) for max-revision computation
// during auto-bump.
func (c *Client) ListByNameArch(ctx context.Context, repo, name, arch string) ([]Package, error) {
	return c.QueryPackages(ctx, repo, fmt.Sprintf("%s {%s}", name, arch))
}

// UploadFile uploads a single .deb to aptly's staging directory dir
// via the File Upload API. Aptly creates dir if absent. Returns the
// list of staged paths aptly reports (typically one entry of the form
// "<dir>/<basename>").
//
// The multipart field name is "file" — matches aptly's example and is
// the form aptly's handler accepts. (aptly is permissive about field
// names but "file" is the documented one.)
func (c *Client) UploadFile(ctx context.Context, dir, localPath string) ([]string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	part, err := mp.CreateFormFile("file", filepath.Base(localPath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	if err := mp.Close(); err != nil {
		return nil, err
	}

	u := fmt.Sprintf("%s/api/files/%s", c.baseURL, url.PathEscape(dir))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mp.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("aptly POST %s: %d %s", req.URL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out []string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("aptly decode: %w", err)
	}
	return out, nil
}

// ImportReport mirrors aptly's POST /api/repos/{name}/file/{dir}
// response body shape. Added/Removed/Warnings carry human-readable
// messages; FailedFiles names files aptly couldn't process.
type ImportReport struct {
	FailedFiles []string `json:"FailedFiles"`
	Report      struct {
		Warnings []string `json:"Warnings"`
		Added    []string `json:"Added"`
		Removed  []string `json:"Removed"`
	} `json:"Report"`
}

// ImportFromDir promotes every uploaded file in dir into the named
// local repository. When forceReplace is true, aptly removes any
// conflicting package already in the repo before importing (used for
// idempotent re-runs).
//
// On a successful HTTP response, callers should still inspect
// ImportReport.FailedFiles — aptly returns 200 even when individual
// files failed to import.
func (c *Client) ImportFromDir(ctx context.Context, repo, dir string, forceReplace bool) (*ImportReport, error) {
	u := fmt.Sprintf("%s/api/repos/%s/file/%s", c.baseURL, url.PathEscape(repo), url.PathEscape(dir))
	if forceReplace {
		u += "?forceReplace=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("aptly POST %s: %d %s", req.URL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out ImportReport
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("aptly decode: %w", err)
	}
	return &out, nil
}

// PublishUpdateOpts controls publish-update behavior. Drayman's
// current surface only needs the signing toggle; pass GPG details via
// a follow-up extension when real deployments demand inline signing
// from the orchestrator (typically aptly is configured server-side).
type PublishUpdateOpts struct {
	// SkipSigning sets `Signing.Skip` on the update request. Needed
	// when the publication was created with skip-signing or when the
	// aptly server lacks a usable GPG key. Without this aptly errors
	// "unable to detached sign file" even if gpgDisableSign is set
	// in the server config (per-publication state wins).
	SkipSigning bool
}

// ErrPublishNotFound signals that the publication at (prefix,
// distribution) doesn't exist yet — typically returned from
// PublishUpdate against a fresh aptly repo. The backend pairs this
// with a fallback PublishRepo call to do the initial create.
//
// Detection is by HTTP 404 alone; aptly's body text ("unable to
// update: published repo with storage:prefix/distribution …") is not
// part of the contract.
var ErrPublishNotFound = errors.New("aptly: publication not found")

// PublishUpdate triggers regeneration of Release/Packages files for
// the publication at (prefix, distribution). aptly re-reads the bound
// local repository's current state, signs/re-emits metadata, and
// writes the new files into the publish endpoint.
//
// Returns ErrPublishNotFound when the publication doesn't exist yet;
// callers (Backend.Publish) recover by doing an initial PublishRepo.
func (c *Client) PublishUpdate(ctx context.Context, prefix, distribution string, opts PublishUpdateOpts) error {
	body := map[string]any{}
	if opts.SkipSigning {
		body["Signing"] = map[string]any{"Skip": true}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/api/publish/%s/%s", c.baseURL, encodeAptlyPrefix(prefix), url.PathEscape(distribution))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: PUT %s: %s", ErrPublishNotFound, req.URL, strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("aptly PUT %s: %d %s", req.URL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// PublishRepoOpts controls the initial POST /api/publish/{prefix} call.
// SourceRepo is the local repository's name; SkipSigning mirrors the
// PublishUpdate flag and pins the publication's signing state for
// every subsequent update.
type PublishRepoOpts struct {
	SourceRepo   string
	Distribution string
	SkipSigning  bool
}

// PublishRepo creates the publication at (prefix, opts.Distribution)
// sourced from opts.SourceRepo. This is the first-publish bootstrap:
// aptly distinguishes "create publication" (POST /api/publish/{prefix})
// from "regenerate published metadata" (PUT /api/publish/{prefix}/{dist}),
// and the PUT 404s when no publication exists yet.
//
// Architectures are deliberately omitted from the body — aptly infers
// them from the source repo's current package set. Component defaults
// to "main" (aptly's default when Sources[].Component is omitted).
func (c *Client) PublishRepo(ctx context.Context, prefix string, opts PublishRepoOpts) error {
	body := map[string]any{
		"SourceKind":   "local",
		"Sources":      []map[string]any{{"Name": opts.SourceRepo}},
		"Distribution": opts.Distribution,
	}
	if opts.SkipSigning {
		body["Signing"] = map[string]any{"Skip": true}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/api/publish/%s", c.baseURL, encodeAptlyPrefix(prefix))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("aptly POST %s: %d %s", req.URL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
