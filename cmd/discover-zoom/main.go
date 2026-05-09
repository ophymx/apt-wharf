// Command discover-zoom is an apt-signpost external discoverer for the Zoom
// Linux client. Implements the stdio JSON contract documented in design.md.
//
//	stdin:  {"prev": {"url": "...", "token": "..."}}   (omitted on cold start)
//	stdout: {"url": "...", "token": "..."}             on change
//	stdout: {"unchanged": true}                        on no-op
//	stderr: free-form diagnostics
//	exit:   0 on success, non-zero on error
//
// The Zoom version metadata at https://zoom.us/rest/download?os=linux carries
// a version string that maps directly to the published .deb URL
// https://zoom.us/client/<version>/zoom_amd64.deb, so the version doubles as
// the change-detection token.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMetadataURL = "https://zoom.us/rest/download?os=linux"
	defaultAssetURL    = "https://zoom.us/client/%s/zoom_amd64.deb"
	defaultTimeout     = 30 * time.Second
)

type input struct {
	Prev *probe `json:"prev,omitempty"`
}

type probe struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type output struct {
	URL       string `json:"url,omitempty"`
	Token     string `json:"token,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

// zoomMeta is a partial decoding of the metadata response — extra fields are
// ignored so unrelated additions on Zoom's side don't break us.
type zoomMeta struct {
	Status       bool   `json:"status"`
	ErrorMessage string `json:"errorMessage"`
	Result       struct {
		DownloadVO struct {
			Zoom struct {
				Version string `json:"version"`
			} `json:"zoom"`
		} `json:"downloadVO"`
	} `json:"result"`
}

// versionPattern matches Zoom's dotted version strings (e.g. "6.5.7.3298").
// Anchored both ends to reject stray whitespace or path separators that would
// be unsafe to interpolate into a URL.
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,4}$`)

func main() {
	metadataURL := flag.String("metadata-url", defaultMetadataURL,
		"Zoom version metadata endpoint")
	assetURL := flag.String("asset-url", defaultAssetURL,
		"asset URL template; %s is replaced by the discovered version")
	timeout := flag.Duration("timeout", defaultTimeout,
		"HTTP timeout for the metadata fetch")
	flag.Parse()

	if err := run(os.Stdin, os.Stdout, *metadataURL, *assetURL, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "discover-zoom:", err)
		os.Exit(1)
	}
}

func run(stdin io.Reader, stdout io.Writer, metadataURL, assetURL string, timeout time.Duration) error {
	if !strings.Contains(assetURL, "%s") {
		return errors.New("--asset-url must contain %s for the version substitution")
	}

	in, err := readInput(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	client := &http.Client{Timeout: timeout}
	version, err := fetchVersion(client, metadataURL)
	if err != nil {
		return err
	}

	out := output{
		URL:   fmt.Sprintf(assetURL, version),
		Token: version,
	}
	if in.Prev != nil && in.Prev.Token == out.Token {
		out = output{Unchanged: true}
	}

	enc := json.NewEncoder(stdout)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

// readInput accepts either nothing, an empty document, or {"prev": {...}}.
// An empty stdin is the cold-start signal per the design contract.
func readInput(r io.Reader) (*input, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	in := &input{}
	if len(strings.TrimSpace(string(body))) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(body, in); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return in, nil
}

func fetchVersion(client *http.Client, metadataURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", metadataURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: status %s", metadataURL, resp.Status)
	}

	var meta zoomMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if !meta.Status {
		msg := meta.ErrorMessage
		if msg == "" {
			msg = "(no errorMessage)"
		}
		return "", fmt.Errorf("metadata response status=false: %s", msg)
	}
	v := strings.TrimSpace(meta.Result.DownloadVO.Zoom.Version)
	if v == "" {
		return "", errors.New("metadata response missing result.downloadVO.zoom.version")
	}
	if !versionPattern.MatchString(v) {
		return "", fmt.Errorf("version %q does not look like a dotted version", v)
	}
	return v, nil
}
