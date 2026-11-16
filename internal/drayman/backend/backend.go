// Package backend defines the apt-repository operations drayman
// needs, abstracted across concrete repo managers. Implementations
// live under backend/aptly (HTTP/REST against aptly) and
// backend/reprepro (subprocess/ssh against reprepro).
//
// Each Backend instance is bound at construction to a single
// (repository, distribution) target — the methods don't take repo
// names so the same interface fits aptly (which has named local
// repos) and reprepro (which has a basedir + distribution).
package backend

import "context"

// Package is the subset of Debian package metadata drayman cares
// about across every backend. Concrete backends carry more fields
// internally (aptly returns SHA256/MD5/Filename etc. on its details
// listing; reprepro's Packages file has the full control stanza);
// the Backend interface returns only what's needed for policy
// decisions and audit log lines.
type Package struct {
	Name            string
	Version         string
	Architecture    string
	BuildInputsHash string // X-Cooper-Build-Inputs-Hash; empty if absent

	// Ref is a backend-opaque handle for this exact repo entry,
	// populated by ListByNameArch and consumed by Remove. aptly fills
	// it with the package Key its delete endpoint wants; reprepro
	// leaves it empty and removes by (Name, Version, Architecture).
	// Callers never interpret it.
	Ref string
}

// Backend is the apt-repository manager drayman targets. Two
// implementations ship: aptly (HTTP) and reprepro (subprocess, local
// or remote via ssh).
//
// All methods are safe to call from a single goroutine and not
// guaranteed safe for concurrent use; drayman reconciles
// single-threaded per run.
type Backend interface {
	// HashExists reports whether a package with the given
	// X-Cooper-Build-Inputs-Hash is already in the target repo.
	HashExists(ctx context.Context, hash string) (bool, error)

	// ListByNameArch returns every package in the target repo with
	// the given Debian Name and Architecture. Empty result is not
	// an error.
	ListByNameArch(ctx context.Context, name, arch string) ([]Package, error)

	// ListByName returns every package in the target repo with the
	// given Debian Name, across all architectures. Used where the
	// caller doesn't pin an architecture (`drayman has` without
	// --arch). Empty result is not an error.
	ListByName(ctx context.Context, name string) ([]Package, error)

	// Import places a built .deb into the target repo. Some
	// backends (reprepro) publish atomically; others (aptly) stage
	// the file for a later Publish. drayman calls Import for each
	// .deb, then Publish once at the end.
	Import(ctx context.Context, debPath string) error

	// Remove deletes one entry (as returned by ListByNameArch) from
	// the target repo. Used by the retain policy to prune superseded
	// versions. Like Import, aptly stages the change for the next
	// Publish while reprepro applies it atomically.
	Remove(ctx context.Context, pkg Package) error

	// Publish regenerates the repo's published Release/Packages
	// metadata. Called once per drayman run after every Import and
	// Remove. Backends that publish atomically per operation return
	// nil here.
	Publish(ctx context.Context) error
}

// Cleaner is implemented by backends that keep a removed entry's
// file around until an explicit garbage-collection pass. aptly is
// the one today: Remove drops the repo entry, and only `db cleanup`
// deletes the pool file. reprepro deletes unreferenced files itself
// and does not implement it.
type Cleaner interface {
	// Cleanup reclaims storage no longer referenced by any repo
	// entry. It may act beyond the bound repo (aptly's cleanup is
	// instance-wide).
	Cleanup(ctx context.Context) error
}

// Querier is the read-only subset of Backend used by the dedup
// policy. Defined here so tests can stub it without a full Backend.
type Querier interface {
	HashExists(ctx context.Context, hash string) (bool, error)
	ListByNameArch(ctx context.Context, name, arch string) ([]Package, error)
	ListByName(ctx context.Context, name string) ([]Package, error)
}
