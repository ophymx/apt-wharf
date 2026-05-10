package source

import (
	"os"

	"github.com/google/go-github/v86/github"
)

func stringPtr(s string) *string { return &s }
func intPtr(i int) *int          { return &i }

// ghAsset constructs a *github.ReleaseAsset shaped like the API response
// — with name, digest, browser_download_url, and a token size.
func ghAsset(name, digest string) *github.ReleaseAsset {
	a := &github.ReleaseAsset{
		Name: stringPtr(name),
		Size: intPtr(123),
	}
	if digest != "" {
		a.Digest = stringPtr(digest)
	}
	url := "https://example.invalid/" + name
	a.BrowserDownloadURL = &url
	return a
}

func writeAll(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
