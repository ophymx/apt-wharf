package export

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
)

// ManifestName is the file the "manifest" target writes at the output
// root: a JSON description of every file and redirect in the export, for
// deploy tooling that targets hosts without a built-in redirect format
// (S3 website redirects, CDN functions, ...).
const ManifestName = "signpost-export.json"

// ManifestSchemaVersion bumps on incompatible manifest shape changes.
const ManifestSchemaVersion = 1

// Cache classes advertised per file. Metadata must be revalidated on
// every apt update; immutable paths are content-addressed (by-hash) or
// version-addressed (pool) and never change in place.
const (
	CacheMetadata  = "metadata"
	CacheImmutable = "immutable"
)

// Manifest is the host-neutral description of an export. Every Path is
// the public URL path relative to the host root, i.e. PathPrefix already
// prepended; deploy tooling can use paths verbatim.
type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	BuiltAt       time.Time        `json:"built_at"`
	PathPrefix    string           `json:"path_prefix"`
	Files         []FileRecord     `json:"files"`
	Redirects     []RedirectRecord `json:"redirects"`
}

// FileRecord describes one static file. Path is URL-absolute and
// prefixed ("/apt/dists/..." for base_url https://host/apt).
type FileRecord struct {
	Path         string     `json:"path"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	ContentType  string     `json:"content_type,omitempty"`
	LastModified *time.Time `json:"last_modified,omitempty"`
	Cache        string     `json:"cache"`

	srcPath string // unprefixed snapshot key, for Write to look up bytes
}

// RedirectRecord describes one redirect. Location is either an absolute
// URL (upstream .deb) or a URL-absolute path (self-referential, already
// carrying the base_url path prefix). Status is always 302: the targets
// are versioned pool paths whose upstream may move between releases.
type RedirectRecord struct {
	Path     string `json:"path"`
	Location string `json:"location"`
	Status   int    `json:"status"`
}

// BuildManifest validates the snapshot's paths and URLs, prepends prefix
// to every path, and returns the sorted manifest. Every target consumes
// this rather than the raw maps, so the validation here is the single
// gate for "can this be exported".
func BuildManifest(snap *refresh.Snapshot, prefix string) (*Manifest, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, BuiltAt: snap.BuiltAt.UTC(), PathPrefix: prefix}
	for p, f := range snap.Files {
		if err := checkPath(prefix + p); err != nil {
			return nil, fmt.Errorf("export: file %w", err)
		}
		rec := FileRecord{
			Path:        prefix + p,
			Size:        int64(len(f.Data)),
			SHA256:      hexSHA256(f.Data),
			ContentType: f.ContentType,
			Cache:       cacheClass(p),
			srcPath:     p,
		}
		if !f.LastModified.IsZero() {
			lm := f.LastModified.UTC()
			rec.LastModified = &lm
		}
		m.Files = append(m.Files, rec)
	}
	for p, rd := range snap.Redirects {
		if err := checkPath(prefix + p); err != nil {
			return nil, fmt.Errorf("export: redirect %w", err)
		}
		if _, dup := snap.Files[p]; dup {
			return nil, fmt.Errorf("export: %s is both a file and a redirect", p)
		}
		if err := checkLocation(rd.URL); err != nil {
			return nil, fmt.Errorf("export: redirect %s: %w", p, err)
		}
		m.Redirects = append(m.Redirects, RedirectRecord{Path: prefix + p, Location: rd.URL, Status: 302})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Slice(m.Redirects, func(i, j int) bool { return m.Redirects[i].Path < m.Redirects[j].Path })
	return m, nil
}

// checkPrefix accepts "" or a "/seg[/seg...]" path with the same
// character hygiene as checkPath. config validation already guarantees
// this shape for repository.base_url; this is the package's own gate.
func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("path prefix %q must not end with /", prefix)
	}
	if err := checkPath(prefix); err != nil {
		return fmt.Errorf("path prefix: %w", err)
	}
	return nil
}

// cacheClass reports how an (unprefixed) path may be cached. by-hash
// entries and pool .debs are addressed by content or version and never
// change in place; everything else is canonical metadata that must
// revalidate.
func cacheClass(p string) string {
	if strings.Contains(p, "/by-hash/") || strings.HasPrefix(p, "/pool/") {
		return CacheImmutable
	}
	return CacheMetadata
}

// checkPath rejects paths that cannot be expressed safely in every
// target's rule syntax. Whitespace breaks line-oriented formats; ':' and
// '*' are placeholder/splat syntax on Cloudflare and Netlify; '"' breaks
// nginx map quoting; non-ASCII and control bytes are simply refused.
// Note the ':' rule means an epoch in Version ("1:2.3") is not exportable
// until signpost encodes it (as %3a, like dpkg) in pool paths.
func checkPath(p string) error {
	if _, err := relPath(p); err != nil {
		return err
	}
	for _, r := range p {
		switch {
		case r <= ' ' || r >= 0x7f:
			return fmt.Errorf("path %q contains whitespace, control, or non-ASCII characters", p)
		case r == ':' || r == '*' || r == '"' || r == '#':
			return fmt.Errorf("path %q contains %q, which is not expressible in redirect rule syntax", p, r)
		}
	}
	return nil
}

// checkLocation accepts an absolute http(s) URL or a URL-absolute path,
// with the same character hygiene as checkPath (minus ':' which URLs need).
func checkLocation(loc string) error {
	if loc == "" {
		return fmt.Errorf("empty redirect location")
	}
	for _, r := range loc {
		if r <= ' ' || r >= 0x7f || r == '"' {
			return fmt.Errorf("location %q contains whitespace, control, non-ASCII, or quote characters", loc)
		}
	}
	if strings.HasPrefix(loc, "/") {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil {
		return fmt.Errorf("location %q: %w", loc, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("location %q is neither an absolute http(s) URL nor an absolute path", loc)
	}
	return nil
}

// manifestTarget writes ManifestName at the output root.
type manifestTarget struct{}

func (manifestTarget) Name() string { return "manifest" }

func (manifestTarget) Emit(m *Manifest) (map[string][]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{ManifestName: append(body, '\n')}, nil
}
