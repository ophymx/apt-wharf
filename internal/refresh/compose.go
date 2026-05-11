package refresh

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/ophymx/apt-signpost/internal/bootstrap"
	"github.com/ophymx/apt-signpost/internal/fetch"
	"github.com/ophymx/apt-signpost/internal/index"
	"github.com/ophymx/apt-signpost/internal/store"
)

// suiteSegment is the URL segment under /dists/. MVP hardcodes "stable".
const suiteSegment = "stable"

// composeSnapshot does steps 4-8 of the refresh pipeline:
//
//   - assemble per-arch Packages + Release
//   - sign → InRelease + Release.gpg
//   - lay out files (canonical + by-hash) and redirects
//   - carry over non-expired entries from prev that aren't already present
func (r *Refresher) composeSnapshot(states map[string]*store.SourceState, bs *store.BootstrapState, debBytes []byte, prev *Snapshot, now time.Time) (*Snapshot, error) {
	entries := make([]*index.Entry, 0, len(states)+1)
	redirects := map[string]Redirect{}

	for name, s := range states {
		if s.Control == "" {
			r.log.Warn("source has no control stanza; skipping", "source", name)
			continue
		}
		fields, err := index.ExtractFields([]byte(s.Control))
		if err != nil {
			r.log.Error("source control parse failed; skipping", "source", name, "err", err)
			continue
		}
		poolPath := poolPath(fields.Package, fields.Version, fields.Architecture)
		entries = append(entries, &index.Entry{
			SourceName: name,
			Control:    []byte(s.Control),
			PoolPath:   poolPath,
			Size:       s.AssetSize,
			SHA256:     s.AssetSHA256,
			Fields:     fields,
		})
		// Pool path → upstream URL (302 redirect).
		redirects[poolPath] = Redirect{URL: s.AssetURL, ExpiresAt: now.Add(Retention)}
	}

	// Bootstrap stanza injection. We extract control verbatim from the
	// .deb nfpm just built — same path every other source uses — so the
	// stanza apt parses out of Packages exactly matches what dpkg records
	// from the .deb. Hand-rolling here previously omitted Installed-Size
	// and reordered fields, causing apt to flag a phantom upgrade.
	bsEntry, err := r.makeBootstrapEntry(bs, debBytes)
	if err != nil {
		return nil, fmt.Errorf("bootstrap entry: %w", err)
	}
	entries = append(entries, bsEntry)

	suite := index.Suite{
		Origin:        r.cfg.Repository.Origin,
		Label:         r.cfg.Repository.Label,
		Codename:      r.cfg.Suite.Codename,
		Description:   r.cfg.Suite.Description,
		Component:     "main",
		Architectures: r.cfg.Suite.Architectures,
	}

	built, err := index.Build(suite, entries, now)
	if err != nil {
		return nil, err
	}

	files := map[string]FileEntry{}

	// Per-arch metadata + by-hash entries. We emit Packages plus three
	// compressed variants (.gz, .xz, .zst) so apt clients can fetch
	// whichever matches their CompressionTypes::Order; each compressed
	// form has its own by-hash entry keyed off the hash listed in Release.
	for arch, af := range built.PerArch {
		canonicalPkg := fmt.Sprintf("/dists/%s/main/binary-%s/Packages", suiteSegment, arch)
		byHashPrefix := fmt.Sprintf("/dists/%s/main/binary-%s/by-hash/SHA256/", suiteSegment, arch)

		variants := []struct {
			suffix      string
			data        []byte
			contentType string
		}{
			{"", af.Packages, "text/plain"},
			{".gz", af.PackagesGz, "application/gzip"},
			{".xz", af.PackagesXz, "application/x-xz"},
			{".zst", af.PackagesZst, "application/zstd"},
		}
		for _, v := range variants {
			files[canonicalPkg+v.suffix] = FileEntry{Data: v.data, ContentType: v.contentType}
			hash := built.PackagesSHA256[fmt.Sprintf("main/binary-%s/Packages%s", arch, v.suffix)]
			files[byHashPrefix+hash] = FileEntry{
				Data:        v.data,
				ContentType: v.contentType,
				ExpiresAt:   now.Add(Retention),
			}
		}
	}

	// Sign Release into InRelease + Release.gpg.
	releaseBytes := built.ReleaseUnsigned
	var inRelease bytes.Buffer
	if err := r.signer.Clearsign(&inRelease, releaseBytes); err != nil {
		return nil, fmt.Errorf("clearsign: %w", err)
	}
	var releaseGpg bytes.Buffer
	if err := r.signer.DetachedSign(&releaseGpg, releaseBytes); err != nil {
		return nil, fmt.Errorf("detached sign: %w", err)
	}

	files[fmt.Sprintf("/dists/%s/Release", suiteSegment)] = FileEntry{Data: releaseBytes, ContentType: "text/plain"}
	files[fmt.Sprintf("/dists/%s/Release.gpg", suiteSegment)] = FileEntry{Data: releaseGpg.Bytes(), ContentType: "application/pgp-signature"}
	files[fmt.Sprintf("/dists/%s/InRelease", suiteSegment)] = FileEntry{Data: inRelease.Bytes(), ContentType: "text/plain"}

	// Bootstrap deb served from its pool path (real bytes, not redirected).
	bsPool := bootstrap.PoolPath(r.cfg.Bootstrap.PackageName, bs.Version)
	files[bsPool] = FileEntry{Data: debBytes, ContentType: "application/vnd.debian.binary-package", ExpiresAt: now.Add(Retention)}

	// Convenience endpoints.
	files["/pubkey.gpg"] = FileEntry{Data: r.signer.KeyringBytes(), ContentType: "application/pgp-keys"}
	files[fmt.Sprintf("/release/%s/latest", suiteSegment)] = FileEntry{Data: []byte(bs.Version + "\n"), ContentType: "text/plain"}
	redirects[fmt.Sprintf("/release/%s/latest.deb", suiteSegment)] = Redirect{URL: bsPool}

	// Carry over non-expired entries from prev that aren't already present.
	if prev != nil {
		for path, e := range prev.Files {
			if _, exists := files[path]; exists {
				continue
			}
			if !e.ExpiresAt.IsZero() && e.ExpiresAt.After(now) {
				files[path] = e
			}
		}
		for path, rd := range prev.Redirects {
			if _, exists := redirects[path]; exists {
				continue
			}
			if !rd.ExpiresAt.IsZero() && rd.ExpiresAt.After(now) {
				redirects[path] = rd
			}
		}
	}

	return &Snapshot{
		Files:     files,
		Redirects: redirects,
		BuiltAt:   now,
	}, nil
}

// makeBootstrapEntry builds the index.Entry for the bootstrap .deb. The
// control stanza comes verbatim from the .deb itself (same as every other
// source), so apt's view of Packages exactly matches dpkg's view of the
// installed package — no Installed-Size drift, no field-ordering drift,
// no hidden Conffiles surprises.
func (r *Refresher) makeBootstrapEntry(bs *store.BootstrapState, debBytes []byte) (*index.Entry, error) {
	pool := bootstrap.PoolPath(r.cfg.Bootstrap.PackageName, bs.Version)
	control, err := extractDebControl(debBytes)
	if err != nil {
		return nil, fmt.Errorf("extract control from bootstrap .deb: %w", err)
	}
	fs, err := index.ExtractFields(control)
	if err != nil {
		return nil, fmt.Errorf("parse bootstrap control: %w", err)
	}
	return &index.Entry{
		SourceName: "bootstrap",
		Control:    control,
		PoolPath:   pool,
		Size:       bs.Size,
		SHA256:     bs.SHA256,
		Fields:     fs,
	}, nil
}

// extractDebControl pulls the inner control file out of an in-memory .deb.
// Reuses the ar/tar/decompressor pipeline from internal/fetch.
func extractDebControl(debBytes []byte) ([]byte, error) {
	if err := fetch.MustHaveARMagic(debBytes); err != nil {
		return nil, err
	}
	members, err := fetch.ParseARHeaders(debBytes)
	if err != nil {
		return nil, err
	}
	ctrl, err := fetch.FindControlMember(members)
	if err != nil {
		return nil, err
	}
	body, ok := fetch.SliceMember(debBytes, ctrl)
	if !ok {
		return nil, fmt.Errorf("control.tar.* extends past .deb body")
	}
	return fetch.ExtractControl(ctrl.Name, body)
}

// poolPath returns the canonical /pool/main/<p>/<name>/<name>_<v>_<a>.deb.
// For names starting with "lib" the prefix is "libX" (Debian convention);
// otherwise it's the first character.
func poolPath(pkg, version, arch string) string {
	prefix := pkg[:1]
	if strings.HasPrefix(pkg, "lib") && len(pkg) >= 4 {
		prefix = pkg[:4]
	}
	return fmt.Sprintf("/pool/main/%s/%s/%s_%s_%s.deb", prefix, pkg, pkg, version, arch)
}
