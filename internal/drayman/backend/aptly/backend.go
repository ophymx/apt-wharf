// Package aptly is the aptly-HTTP-API implementation of
// backend.Backend. It wraps internal/drayman/aptly's client (the
// lower-level HTTP transport) and converts its richer wire-format
// records to backend.Package.
package aptly

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ophymx/apt-wharf/internal/drayman/aptly"
	"github.com/ophymx/apt-wharf/internal/drayman/backend"
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
	return toBackendPackages(pkgs), nil
}

func (b *Backend) ListByName(ctx context.Context, name string) ([]backend.Package, error) {
	pkgs, err := b.client.ListByName(ctx, b.repo, name)
	if err != nil {
		return nil, err
	}
	return toBackendPackages(pkgs), nil
}

func toBackendPackages(pkgs []aptly.Package) []backend.Package {
	out := make([]backend.Package, len(pkgs))
	for i, p := range pkgs {
		out[i] = backend.Package{
			Name:            p.Package,
			Version:         p.Version,
			Architecture:    p.Architecture,
			BuildInputsHash: p.XCooperBuildInputsHash,
			Ref:             p.Key,
		}
	}
	return out
}

// Import uploads every .deb to the aptly staging directory, then
// promotes them into the local repo with one import-from-dir request,
// so a batch lands in a single aptly operation. reconcile calls this
// per artifact, `drayman import` once for the whole set.
//
// On any failure the staging directory is dropped: aptly only deletes
// the files an import consumed, so a rejected (or never-imported)
// upload would otherwise sit in the upload area — and be swept into,
// and fail, the next import that reuses the directory.
func (b *Backend) Import(ctx context.Context, debPaths ...string) (err error) {
	if len(debPaths) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			_ = b.client.DeleteUploadDir(context.WithoutCancel(ctx), b.stagingDir)
		}
	}()
	for _, debPath := range debPaths {
		if _, err := b.client.UploadFile(ctx, b.stagingDir, debPath); err != nil {
			return err
		}
	}
	rep, err := b.client.ImportFromDir(ctx, b.repo, b.stagingDir, true)
	if err != nil {
		return err
	}
	if len(rep.FailedFiles) > 0 {
		return errImport{files: rep.FailedFiles, warnings: rep.Report.Warnings}
	}
	return nil
}

// Remove deletes the entry from the local repo by its aptly Key
// (carried in pkg.Ref). The published tree still lists it until the
// next Publish; aptly's internal pool keeps the file until a db
// cleanup (Cleanup, or `aptly db cleanup` on the host).
func (b *Backend) Remove(ctx context.Context, pkg backend.Package) error {
	if pkg.Ref == "" {
		return fmt.Errorf("aptly remove %s %s %s: entry has no aptly key", pkg.Name, pkg.Version, pkg.Architecture)
	}
	return b.client.RemovePackages(ctx, b.repo, []string{pkg.Ref})
}

// Cleanup runs aptly's db cleanup, which deletes pool files (and
// package entries) no repo, snapshot, or publication references any
// more. It is what frees the disk after Remove, and it acts on the
// whole aptly instance rather than just the bound repo. Call it after
// Publish: until then the publication still references pruned files.
func (b *Backend) Cleanup(ctx context.Context) error {
	return b.client.DBCleanup(ctx)
}

// Publish regenerates the published Release/Packages files for the
// bound (prefix, distribution). aptly stages everything Import did;
// publishing is what makes it visible to apt clients.
//
// First-publish bootstrap: aptly's PUT /api/publish endpoint only
// works on an already-published distribution; a fresh repo 404s.
// When PublishUpdate signals ErrPublishNotFound, we fall back to
// PublishRepo (POST /api/publish/{prefix}) to create the publication
// for the first time, then return. Subsequent runs go straight to
// PublishUpdate (the fast path).
func (b *Backend) Publish(ctx context.Context) error {
	err := b.client.PublishUpdate(ctx, b.prefix, b.distribution, aptly.PublishUpdateOpts{
		SkipSigning: b.skipSigning,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, aptly.ErrPublishNotFound) {
		return err
	}
	return b.client.PublishRepo(ctx, b.prefix, aptly.PublishRepoOpts{
		SourceRepo:   b.repo,
		Distribution: b.distribution,
		SkipSigning:  b.skipSigning,
	})
}

// errImport surfaces the names aptly reported as failed-to-import,
// with the report's warnings: those carry the actual reason.
type errImport struct {
	files    []string
	warnings []string
}

func (e errImport) Error() string {
	msg := "aptly import failed: " + strings.Join(e.files, ", ")
	if len(e.warnings) > 0 {
		msg += " (" + strings.Join(e.warnings, "; ") + ")"
	}
	return msg
}
