package build

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// ExtractOpts bounds the unpacked footprint of an archive. Defaults
// match cooper-design.md §"Security & sandboxing": 1 GiB uncompressed,
// 100 000 entries.
type ExtractOpts struct {
	MaxBytes int64
	MaxFiles int
}

const (
	defaultMaxBytes int64 = 1 << 30
	defaultMaxFiles       = 100_000

	// Build-side hard ceilings, mirroring the validate-time check in
	// internal/cooper/config. Independent enforcement at both layers
	// means an external producer can't sidestep the safety net by
	// emitting a plan with absurd extract values.
	extractCeilingBytes int64 = 64 << 30 // 64 GiB
	extractCeilingFiles       = 2_000_000

	// maxComponentBytes mirrors stage.maxPathComponentBytes — every
	// path component the tarball declares must fit a typical filesystem.
	maxComponentBytes = 255
)

// IsArchive reports whether name's extension matches a format we
// extract. Non-archive assets are left at <staging>/asset/<asset.name>
// and the user's nfpm doc references them via ${ASSETS}/<asset.name>.
func IsArchive(name string) bool {
	lower := strings.ToLower(name)
	for _, suf := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.zst", ".tar.bz2", ".zip"} {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// Extract dispatches on the archive's extension and writes the entries
// directly under destDir. destDir must already exist. The function does
// NOT strip a leading directory — a top-level dir inside the archive
// becomes a top-level dir under destDir, matching the cooper-design.md
// contract that ${ASSETS}/<archive-top-dir> is what nfpm sees.
func Extract(archivePath, destDir string, opts ExtractOpts) error {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.MaxBytes > extractCeilingBytes {
		return fmt.Errorf("extract opts: max_bytes %d exceeds ceiling of %d", opts.MaxBytes, extractCeilingBytes)
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = defaultMaxFiles
	}
	if opts.MaxFiles > extractCeilingFiles {
		return fmt.Errorf("extract opts: max_files %d exceeds ceiling of %d", opts.MaxFiles, extractCeilingFiles)
	}

	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return openAndExtractTar(archivePath, destDir, opts, openGzip)
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return openAndExtractTar(archivePath, destDir, opts, openXz)
	case strings.HasSuffix(lower, ".tar.zst"):
		return openAndExtractTar(archivePath, destDir, opts, openZstd)
	case strings.HasSuffix(lower, ".tar.bz2"):
		return openAndExtractTar(archivePath, destDir, opts, openBzip2)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, destDir, opts)
	default:
		return fmt.Errorf("not an archive: %s", archivePath)
	}
}

type decompressorOpener func(io.Reader) (io.Reader, func() error, error)

func openGzip(r io.Reader) (io.Reader, func() error, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, err
	}
	return gz, gz.Close, nil
}

func openXz(r io.Reader) (io.Reader, func() error, error) {
	xr, err := xz.NewReader(r)
	if err != nil {
		return nil, nil, err
	}
	return xr, func() error { return nil }, nil
}

func openZstd(r io.Reader) (io.Reader, func() error, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, nil, err
	}
	return zr, func() error { zr.Close(); return nil }, nil
}

func openBzip2(r io.Reader) (io.Reader, func() error, error) {
	return bzip2.NewReader(r), func() error { return nil }, nil
}

// openAndExtractTar opens archivePath, applies the named decompressor,
// and walks the inner tar stream. Errors close cleanly; the file handle
// is always released.
func openAndExtractTar(archivePath, destDir string, opts ExtractOpts, open decompressorOpener) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", archivePath, err)
	}
	defer f.Close()

	r, closer, err := open(f)
	if err != nil {
		return fmt.Errorf("decompress %s: %w", archivePath, err)
	}
	defer closer()

	return extractTar(r, destDir, opts)
}

func extractTar(r io.Reader, destDir string, opts ExtractOpts) error {
	tr := tar.NewReader(r)
	var totalBytes int64
	var fileCount int

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	absDest = filepath.Clean(absDest)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar header: %w", err)
		}
		fileCount++
		if fileCount > opts.MaxFiles {
			return fmt.Errorf("archive has more than %d entries", opts.MaxFiles)
		}

		name := hdr.Name
		if err := validateArchiveName(name); err != nil {
			return err
		}
		target := filepath.Join(absDest, filepath.FromSlash(name))
		if !pathInside(absDest, target) {
			return fmt.Errorf("entry %q escapes destination", name)
		}

		rawMode := os.FileMode(hdr.Mode) & 0o7777
		if rawMode&0o6000 != 0 {
			return fmt.Errorf("entry %q has setuid/setgid bits", name)
		}
		mode := rawMode & 0o0777
		if mode == 0 {
			mode = 0o644
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			remaining := opts.MaxBytes - totalBytes
			limited := io.LimitReader(tr, remaining+1)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			n, err := io.Copy(out, limited)
			cerr := out.Close()
			if err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
			if cerr != nil {
				return fmt.Errorf("close %s: %w", target, cerr)
			}
			if n > remaining {
				return fmt.Errorf("archive exceeds %d uncompressed bytes", opts.MaxBytes)
			}
			totalBytes += n

		case tar.TypeSymlink:
			if err := validateLinkTarget(absDest, target, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}

		case tar.TypeLink:
			if err := validateArchiveName(hdr.Linkname); err != nil {
				return err
			}
			src := filepath.Join(absDest, filepath.FromSlash(hdr.Linkname))
			if !pathInside(absDest, src) {
				return fmt.Errorf("hardlink target %q escapes destination", hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(src, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", target, src, err)
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("entry %q is a device/FIFO (rejected)", name)

		default:
			return fmt.Errorf("entry %q has unsupported tar type %d", name, hdr.Typeflag)
		}
	}
}

func extractZip(archivePath, destDir string, opts ExtractOpts) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", archivePath, err)
	}
	defer zr.Close()

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	absDest = filepath.Clean(absDest)

	var totalBytes int64
	var fileCount int

	for _, f := range zr.File {
		fileCount++
		if fileCount > opts.MaxFiles {
			return fmt.Errorf("archive has more than %d entries", opts.MaxFiles)
		}
		name := f.Name
		if err := validateArchiveName(name); err != nil {
			return err
		}
		target := filepath.Join(absDest, filepath.FromSlash(name))
		if !pathInside(absDest, target) {
			return fmt.Errorf("entry %q escapes destination", name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		mode := f.Mode().Perm()
		if mode&0o6000 != 0 {
			return fmt.Errorf("entry %q has setuid/setgid bits", name)
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file (zip type %v)", name, f.Mode().Type())
		}
		if mode == 0 {
			mode = 0o644
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open %s in zip: %w", name, err)
		}
		remaining := opts.MaxBytes - totalBytes
		limited := io.LimitReader(rc, remaining+1)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", target, err)
		}
		n, err := io.Copy(out, limited)
		closeErr := out.Close()
		rcErr := rc.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", target, closeErr)
		}
		if rcErr != nil {
			return fmt.Errorf("close %s in zip: %w", name, rcErr)
		}
		if n > remaining {
			return fmt.Errorf("archive exceeds %d uncompressed bytes", opts.MaxBytes)
		}
		totalBytes += n
	}
	return nil
}

// validateArchiveName enforces the lexical rules from
// cooper-design.md §"Security & sandboxing": no NUL, no absolute paths,
// no .. components, per-component byte cap.
func validateArchiveName(name string) error {
	if name == "" {
		return fmt.Errorf("empty entry name")
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("entry name contains NUL")
	}
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return fmt.Errorf("entry name %q is absolute", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("entry name %q traverses up", name)
	}
	for comp := range strings.SplitSeq(cleaned, "/") {
		if comp == ".." {
			return fmt.Errorf("entry name %q has .. component", name)
		}
		if len(comp) > maxComponentBytes {
			return fmt.Errorf("entry name has component longer than %d bytes", maxComponentBytes)
		}
	}
	return nil
}

// pathInside reports whether target is inside (or equal to) root, after
// lexical cleaning. Symlinks are NOT followed here — extraction only
// writes new entries under root, so lexical containment suffices.
func pathInside(root, target string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateLinkTarget enforces the symlink-target containment rule. For
// relative targets, resolution is from the symlink's parent directory.
// We're checking the lexical destination, not whether it currently
// exists — entries earlier in the tarball may not have created the
// target yet.
func validateLinkTarget(absDest, linkPath, linkTarget string) error {
	if strings.ContainsRune(linkTarget, 0) {
		return fmt.Errorf("symlink target contains NUL")
	}
	var resolved string
	if filepath.IsAbs(linkTarget) || path.IsAbs(linkTarget) {
		resolved = filepath.Clean(linkTarget)
	} else {
		resolved = filepath.Clean(filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(linkTarget)))
	}
	if !pathInside(absDest, resolved) {
		return fmt.Errorf("symlink %s -> %s escapes destination", linkPath, linkTarget)
	}
	return nil
}
