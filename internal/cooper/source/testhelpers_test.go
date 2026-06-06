package source

import (
	"os"

	"github.com/google/go-github/v86/github"
)

//go:fix inline
func stringPtr(s string) *string { return new(s) }

//go:fix inline
func intPtr(i int) *int { return new(i) }

// ghAsset constructs a *github.ReleaseAsset shaped like the API response
// — with name, digest, browser_download_url, and a token size.
func ghAsset(name, digest string) *github.ReleaseAsset {
	a := &github.ReleaseAsset{
		Name: new(name),
		Size: new(123),
	}
	if digest != "" {
		a.Digest = new(digest)
	}
	url := "https://example.invalid/" + name
	a.BrowserDownloadURL = &url
	return a
}

func writeAll(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
