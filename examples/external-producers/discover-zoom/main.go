// Command discover-zoom is an apt-signpost external discoverer for the Zoom
// Linux client. The wire protocol is implemented by the
// github.com/ophymx/apt-wharf/external package; this binary just maps a
// version string out of Zoom's metadata endpoint to the published .deb URL.
//
// The metadata at https://zoom.us/rest/download?os=linux carries
// result.downloadVO.zoom.version, which appears verbatim in the asset URL
// https://zoom.us/client/<version>/zoom_amd64.deb, so the version doubles
// as the change-detection token.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ophymx/apt-wharf/external"
)

const assetURL = "https://zoom.us/client/%s/zoom_amd64.deb"

// var rather than const so tests can swap in an httptest URL and a tighter
// timeout. Production main never mutates these.
var (
	metadataURL = "https://zoom.us/rest/download?os=linux"
	httpTimeout = 30 * time.Second
)

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
// Anchored both ends to reject stray whitespace or path separators that
// would be unsafe to interpolate into a URL.
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,4}$`)

func main() {
	if err := external.Run(discover); err != nil {
		fmt.Fprintln(os.Stderr, "discover-zoom:", err)
		os.Exit(1)
	}
}

// discover is the testable core. metadataURL and timeout are parameters
// solely so httptest can inject a stand-in; production main always passes
// the package constants.
func discover(ctx context.Context, prev *external.Probe) (external.Output, error) {
	client := &http.Client{Timeout: httpTimeout}
	version, err := fetchVersion(ctx, client, metadataURL)
	if err != nil {
		return external.Output{}, err
	}
	if prev != nil && prev.Token == version {
		return external.Output{Unchanged: true}, nil
	}
	return external.Output{
		URL:   fmt.Sprintf(assetURL, version),
		Token: version,
	}, nil
}

func fetchVersion(ctx context.Context, client *http.Client, metaURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", metaURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: status %s", metaURL, resp.Status)
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
