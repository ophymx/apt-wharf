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

	// Import places a built .deb into the target repo. Some
	// backends (reprepro) publish atomically; others (aptly) stage
	// the file for a later Publish. drayman calls Import for each
	// .deb, then Publish once at the end.
	Import(ctx context.Context, debPath string) error

	// Publish regenerates the repo's published Release/Packages
	// metadata. Called once per drayman run after every Import.
	// Backends that publish atomically per Import return nil here.
	Publish(ctx context.Context) error
}

// Querier is the read-only subset of Backend used by the dedup
// policy. Defined here so tests can stub it without a full Backend.
type Querier interface {
	HashExists(ctx context.Context, hash string) (bool, error)
	ListByNameArch(ctx context.Context, name, arch string) ([]Package, error)
}
