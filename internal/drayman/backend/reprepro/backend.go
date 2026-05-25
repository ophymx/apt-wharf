// Package reprepro implements backend.Backend against reprepro,
// either on the local filesystem or on a remote host over ssh/scp.
//
// reprepro is a CLI tool, not a daemon — drayman invokes it as a
// subprocess (`reprepro -b <basedir> includedeb <dist> <file>`) per
// import. Unlike aptly, reprepro publishes Release/Packages
// atomically on every includedeb, so Backend.Publish is a no-op.
//
// Queries go against the on-disk dists/<dist>/<component>/binary-<arch>/Packages
// files reprepro maintains; drayman parses them directly. No reprepro
// subprocess is needed for read operations.
package reprepro

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
)

// Backend talks to a reprepro repository (local or remote).
// Construct with NewLocal or NewSSH.
type Backend struct {
	transport    transport
	basedir      string
	distribution string
	component    string

	cache    []backend.Package
	cacheErr error
	cached   bool
}

// Opts is the construction-time configuration shared by both Local
// and SSH constructors. SSHDest is empty for the local case.
type Opts struct {
	// Basedir is reprepro's -b argument; the directory containing
	// conf/distributions, db/, dists/, pool/.
	Basedir string
	// Distribution is the apt distribution name (e.g. "stable").
	// Must match an entry in conf/distributions.
	Distribution string
	// Component defaults to "main" when empty.
	Component string
	// SSHDest, when non-empty, switches to the ssh transport and
	// names the destination (user@host or an ssh_config alias).
	SSHDest string
}

// New constructs a Backend with the appropriate transport.
// SSHDest == "" picks the local transport; otherwise ssh.
func New(opts Opts) *Backend {
	if opts.Component == "" {
		opts.Component = "main"
	}
	var t transport = localTransport{}
	if opts.SSHDest != "" {
		t = sshTransport{dest: opts.SSHDest}
	}
	return &Backend{
		transport:    t,
		basedir:      opts.Basedir,
		distribution: opts.Distribution,
		component:    opts.Component,
	}
}

// NewLocal is a convenience constructor for the local transport.
func NewLocal(basedir, distribution, component string) *Backend {
	return New(Opts{Basedir: basedir, Distribution: distribution, Component: component})
}

// NewSSH is a convenience constructor for the ssh transport.
func NewSSH(dest, basedir, distribution, component string) *Backend {
	return New(Opts{SSHDest: dest, Basedir: basedir, Distribution: distribution, Component: component})
}

// HashExists scans every Packages file under the distribution and
// reports whether any record carries the given hash.
func (b *Backend) HashExists(ctx context.Context, hash string) (bool, error) {
	pkgs, err := b.snapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pkgs {
		if p.BuildInputsHash == hash {
			return true, nil
		}
	}
	return false, nil
}

// ListByNameArch returns every record in the distribution matching
// the given (Name, Architecture).
func (b *Backend) ListByNameArch(ctx context.Context, name, arch string) ([]backend.Package, error) {
	pkgs, err := b.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var out []backend.Package
	for _, p := range pkgs {
		if p.Name == name && p.Architecture == arch {
			out = append(out, p)
		}
	}
	return out, nil
}

// Import runs `reprepro -b <basedir> includedeb <dist> <file>` on
// the target. For the ssh transport, debPath is first scp'd to a
// temp location on the remote host.
//
// reprepro publishes atomically on every includedeb, so the
// repo's published metadata reflects the new package immediately —
// no separate Publish step is needed.
func (b *Backend) Import(ctx context.Context, debPath string) error {
	remote, err := b.transport.stageFile(ctx, debPath)
	if err != nil {
		return fmt.Errorf("stage %s: %w", debPath, err)
	}
	defer func() {
		_ = b.transport.removeFile(ctx, remote)
	}()
	if _, err := b.transport.runCmd(ctx, "reprepro", "-b", b.basedir, "includedeb", b.distribution, remote); err != nil {
		return fmt.Errorf("reprepro includedeb: %w", err)
	}
	// Invalidate the snapshot cache so the next query sees the
	// just-imported package.
	b.cached = false
	b.cache = nil
	b.cacheErr = nil
	return nil
}

// Publish is a no-op for reprepro: includedeb already regenerated
// the published metadata atomically.
func (b *Backend) Publish(_ context.Context) error {
	return nil
}

// snapshot lazily reads every Packages file under
// dists/<dist>/<component>/ once per Backend lifetime (or until
// Import invalidates the cache).
func (b *Backend) snapshot(ctx context.Context) ([]backend.Package, error) {
	if b.cached {
		return b.cache, b.cacheErr
	}
	b.cache, b.cacheErr = b.fetchSnapshot(ctx)
	b.cached = true
	return b.cache, b.cacheErr
}

func (b *Backend) fetchSnapshot(ctx context.Context) ([]backend.Package, error) {
	root := filepath.Join(b.basedir, "dists", b.distribution, b.component)
	paths, err := b.transport.listPackagesFiles(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("list packages files under %s: %w", root, err)
	}
	var all []backend.Package
	for _, p := range paths {
		body, err := b.transport.readFile(ctx, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		all = append(all, parsePackages(body)...)
	}
	return all, nil
}
