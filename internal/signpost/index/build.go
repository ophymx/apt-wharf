// Package index assembles per-arch Packages and the suite-level Release.
//
// The strict checks per design-mvp.md run before any output is produced:
//   - Parsed Architecture must be "all" or in suite.Architectures.
//   - No two entries may resolve to the same (Package, Version, Architecture).
//
// All errors are returned together so a refresh tick can log every offender
// in a single batch rather than only the first.
package index

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Entry is one source's contribution to the index.
type Entry struct {
	SourceName string // for log lines
	Control    []byte // verbatim control stanza, no Filename/Size/SHA256 yet
	PoolPath   string // canonical /pool/main/<p>/<name>_<v>_<a>.deb
	Size       int64
	SHA256     string // lowercase hex

	// Cached parse result; ExtractFields is cheap but we don't want to
	// re-run it during strict checks.
	Fields FieldSet
}

// Suite is the input metadata for the Release file.
type Suite struct {
	Origin        string
	Label         string
	Codename      string
	Description   string
	Component     string // "main" in MVP
	Architectures []string
}

// ArchFiles holds the rendered Packages plus its compressed variants
// (gzip, xz, zstd) for a single arch. All three compressed forms are
// surfaced so apt clients with different CompressionTypes::Order can
// pick whichever they prefer; the Release file lists hashes for all
// of them so apt can verify whichever it fetches.
type ArchFiles struct {
	Packages    []byte
	PackagesGz  []byte
	PackagesXz  []byte
	PackagesZst []byte
}

// Built captures all metadata bytes and their hashes. Hashes are surfaced
// because the Release file references them and the refresh layer needs
// them to lay down by-hash paths.
type Built struct {
	PerArch         map[string]ArchFiles
	ReleaseUnsigned []byte

	// PackagesSHA256 is keyed by canonical path
	// "main/binary-<arch>/Packages[.gz|.xz|.zst]".
	PackagesSHA256 map[string]string
	PackagesSize   map[string]int64
}

// Build runs strict checks and returns the assembled metadata bytes.
func Build(s Suite, entries []*Entry, now time.Time) (*Built, error) {
	if err := strictChecks(s, entries); err != nil {
		return nil, err
	}

	perArch := map[string]ArchFiles{}
	hashes := map[string]string{}
	sizes := map[string]int64{}

	// Precompute "all"-arch entries so we can fan them into every binary-<arch>.
	var allArchEntries []*Entry
	byArch := map[string][]*Entry{}
	for _, e := range entries {
		if e.Fields.Architecture == "all" {
			allArchEntries = append(allArchEntries, e)
			continue
		}
		byArch[e.Fields.Architecture] = append(byArch[e.Fields.Architecture], e)
	}

	for _, arch := range s.Architectures {
		archEntries := append([]*Entry{}, byArch[arch]...)
		archEntries = append(archEntries, allArchEntries...)
		sortEntries(archEntries)

		pkg := renderPackages(archEntries)
		gz, err := gzipBytes(pkg)
		if err != nil {
			return nil, fmt.Errorf("gzip Packages for %s: %w", arch, err)
		}
		xzd, err := xzBytes(pkg)
		if err != nil {
			return nil, fmt.Errorf("xz Packages for %s: %w", arch, err)
		}
		zst, err := zstdBytes(pkg)
		if err != nil {
			return nil, fmt.Errorf("zstd Packages for %s: %w", arch, err)
		}
		perArch[arch] = ArchFiles{
			Packages:    pkg,
			PackagesGz:  gz,
			PackagesXz:  xzd,
			PackagesZst: zst,
		}

		pkgPath := fmt.Sprintf("%s/binary-%s/Packages", s.Component, arch)
		for path, data := range map[string][]byte{
			pkgPath:          pkg,
			pkgPath + ".gz":  gz,
			pkgPath + ".xz":  xzd,
			pkgPath + ".zst": zst,
		} {
			hashes[path] = hexSHA256(data)
			sizes[path] = int64(len(data))
		}
	}

	release := renderRelease(s, hashes, sizes, now)

	return &Built{
		PerArch:         perArch,
		ReleaseUnsigned: release,
		PackagesSHA256:  hashes,
		PackagesSize:    sizes,
	}, nil
}

func strictChecks(s Suite, entries []*Entry) error {
	allowed := map[string]struct{}{"all": {}}
	for _, a := range s.Architectures {
		allowed[a] = struct{}{}
	}
	type triple struct{ p, v, a string }
	seen := map[triple][]string{}
	var errs []error

	for _, e := range entries {
		if _, ok := allowed[e.Fields.Architecture]; !ok {
			errs = append(errs, fmt.Errorf(
				"source %s: parsed Architecture %q is not in suite.architectures (%s) and is not \"all\"",
				e.SourceName, e.Fields.Architecture, strings.Join(s.Architectures, ", ")))
			continue
		}
		k := triple{e.Fields.Package, e.Fields.Version, e.Fields.Architecture}
		seen[k] = append(seen[k], e.SourceName)
	}
	for k, srcs := range seen {
		if len(srcs) > 1 {
			sort.Strings(srcs)
			errs = append(errs, fmt.Errorf(
				"duplicate (Package=%s Version=%s Architecture=%s) from sources: %s",
				k.p, k.v, k.a, strings.Join(srcs, ", ")))
		}
	}
	return errors.Join(errs...)
}

// renderPackages emits one Packages file. Each entry contributes:
//   - its verbatim control stanza (already parsed once)
//   - Filename, Size, SHA256 fields appended
//
// Stanzas are separated by a single blank line, terminating with one
// trailing newline.
func renderPackages(entries []*Entry) []byte {
	var buf bytes.Buffer
	for i, e := range entries {
		if i > 0 {
			buf.WriteByte('\n')
		}
		body := bytes.TrimRight(e.Control, "\n")
		buf.Write(body)
		buf.WriteByte('\n')
		fmt.Fprintf(&buf, "Filename: %s\n", strings.TrimPrefix(e.PoolPath, "/"))
		fmt.Fprintf(&buf, "Size: %d\n", e.Size)
		fmt.Fprintf(&buf, "SHA256: %s\n", e.SHA256)
	}
	return buf.Bytes()
}

func renderRelease(s Suite, hashes map[string]string, sizes map[string]int64, now time.Time) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Origin: %s\n", s.Origin)
	fmt.Fprintf(&buf, "Label: %s\n", s.Label)
	fmt.Fprintf(&buf, "Suite: %s\n", s.Codename)
	fmt.Fprintf(&buf, "Codename: %s\n", s.Codename)
	fmt.Fprintf(&buf, "Components: %s\n", s.Component)
	fmt.Fprintf(&buf, "Architectures: %s\n", strings.Join(s.Architectures, " "))
	fmt.Fprintf(&buf, "Description: %s\n", s.Description)
	fmt.Fprintf(&buf, "Date: %s\n", now.UTC().Format(time.RFC1123))
	fmt.Fprintln(&buf, "Acquire-By-Hash: yes")
	fmt.Fprintln(&buf, "SHA256:")

	paths := make([]string, 0, len(hashes))
	for p := range hashes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&buf, " %s %d %s\n", hashes[p], sizes[p], p)
	}
	return buf.Bytes()
}

func gzipBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(in); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xzBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(in); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// zstdBytes pins encoder concurrency to 1 so two invocations on the
// same input produce byte-identical output (klauspost/zstd's parallel
// path can vary block boundaries otherwise).
func zstdBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(in); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sortEntries gives Packages a stable order so two refresh ticks with the
// same inputs produce byte-identical outputs (and hence identical hashes,
// avoiding pointless retention churn).
func sortEntries(entries []*Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Fields.Package != entries[j].Fields.Package {
			return entries[i].Fields.Package < entries[j].Fields.Package
		}
		return entries[i].Fields.Version < entries[j].Fields.Version
	})
}
