// Package aptly is the aptly-HTTP-API implementation of
// backend.Backend. It wraps internal/drayman/aptly's client (the
// lower-level HTTP transport) and converts its richer wire-format
// records to backend.Package.
package aptly

import (
	"context"
	"net/http"
	"strings"

	"github.com/ophymx/apt-signpost/internal/drayman/aptly"
	"github.com/ophymx/apt-signpost/internal/drayman/backend"
)

// Backend talks to a single aptly local repository + publication.
// Construct with New.
type Backend struct {
	client       *aptly.Client
	repo         string
	prefix       string
	distribution string
	stagingDir   string // upload directory name aptly stages files under
	skipSigning  bool
}

// Opts configures a new aptly Backend.
type Opts struct {
	// BaseURL is the aptly API root, e.g. "http://localhost:8080".
	BaseURL string
	// Repo is the aptly local repository name.
	Repo string
	// PublishPrefix is the aptly publication prefix; defaults to "."
	// (no prefix). Aptly's URL convention encodes "." as ":.".
	PublishPrefix string
	// Distribution is the aptly publish distribution (e.g. "stable").
	Distribution string
	// StagingDir is the upload directory aptly stages files under
	// during an Import. Defaults to a generated "drayman-<hex>" name
	// per Backend; reuse is fine since Import drains it.
	StagingDir string
	// SkipSigning passes Signing.Skip on the publish-update request.
	// Used when the aptly server's gpg config is disabled.
	SkipSigning bool
	// HTTPClient is the http.Client used for aptly API calls. Optional;
	// when nil, http.DefaultClient is used. Production callers typically
	// set a longer Timeout than the default (uploads can be slow);
	// tests can point an httptest.Server-aware client at a local fake.
	HTTPClient *http.Client
}

// New constructs a Backend.
func New(opts Opts) *Backend {
	if opts.PublishPrefix == "" {
		opts.PublishPrefix = "."
	}
	return &Backend{
		client:       aptly.New(opts.BaseURL, opts.HTTPClient),
		repo:         opts.Repo,
		prefix:       opts.PublishPrefix,
		distribution: opts.Distribution,
		stagingDir:   opts.StagingDir,
		skipSigning:  opts.SkipSigning,
	}
}

func (b *Backend) HashExists(ctx context.Context, hash string) (bool, error) {
	return b.client.HashExists(ctx, b.repo, hash)
}

func (b *Backend) ListByNameArch(ctx context.Context, name, arch string) ([]backend.Package, error) {
	pkgs, err := b.client.ListByNameArch(ctx, b.repo, name, arch)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Package, len(pkgs))
	for i, p := range pkgs {
		out[i] = backend.Package{
			Name:            p.Package,
			Version:         p.Version,
			Architecture:    p.Architecture,
			BuildInputsHash: p.XCooperBuildInputsHash,
		}
	}
	return out, nil
}

// Import uploads the .deb into the staging directory and immediately
// promotes it into the local repo. Drayman calls this once per
// artifact; the per-file overhead is small (one upload + one
// import-from-dir request per call).
func (b *Backend) Import(ctx context.Context, debPath string) error {
	if _, err := b.client.UploadFile(ctx, b.stagingDir, debPath); err != nil {
		return err
	}
	rep, err := b.client.ImportFromDir(ctx, b.repo, b.stagingDir, true)
	if err != nil {
		return err
	}
	if len(rep.FailedFiles) > 0 {
		return errImport{files: rep.FailedFiles}
	}
	return nil
}

// Publish regenerates the published Release/Packages files for the
// bound (prefix, distribution). aptly stages everything Import did;
// publishing is what makes it visible to apt clients.
func (b *Backend) Publish(ctx context.Context) error {
	return b.client.PublishUpdate(ctx, b.prefix, b.distribution, aptly.PublishUpdateOpts{
		SkipSigning: b.skipSigning,
	})
}

// errImport surfaces the names aptly reported as failed-to-import.
type errImport struct {
	files []string
}

func (e errImport) Error() string {
	return "aptly import failed: " + strings.Join(e.files, ", ")
}
