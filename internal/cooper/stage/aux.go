package stage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Ref is one resolved aux-file reference.
//
// Key is the source-relative path used as the build_plan.aux_files map
// key. For literal-file references and .tmpl renders it is the path doc
// 2 wrote verbatim; for glob and directory references it is the
// "./<rel-to-pkgdir>" form of each inlined match.
//
// AbsPath is the file on disk that supplies the bytes — for .tmpl
// fallback it points at "<P>.tmpl" while Key still points at "<P>".
type Ref struct {
	Key      string
	AbsPath  string
	Template bool
}

const maxPathComponentBytes = 255

// scriptKeys is the closed set of nfpm scripts: subkeys cooper walks for
// path resolution. Anything else under scripts: is an unknown nfpm key
// that nfpm itself will reject at build time.
var scriptKeys = []string{"preinstall", "postinstall", "preremove", "postremove"}

// Walk classifies and resolves every in-scope path in nfpm doc 2 (the
// contents[].src and scripts.{preinstall,postinstall,preremove,postremove}
// scalars). Refs are deduplicated by Key.
//
// Failures map to cooper-design.md §"Aux file resolution"'s hard-error
// list. The first failure aborts the walk for the package.
func Walk(pkgDir string, nfpm *yaml.Node) ([]Ref, error) {
	root, err := documentRoot(nfpm)
	if err != nil {
		return nil, err
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("nfpm doc must be a YAML mapping at top level")
	}

	realPkgDir, err := filepath.EvalSymlinks(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("resolve package dir %q: %w", pkgDir, err)
	}

	var refs []Ref
	seen := map[string]struct{}{}
	emit := func(in []Ref) {
		for _, r := range in {
			if _, dup := seen[r.Key]; dup {
				continue
			}
			seen[r.Key] = struct{}{}
			refs = append(refs, r)
		}
	}

	// contents[].src
	for _, src := range collectContentsSrc(root) {
		got, err := resolve(realPkgDir, src)
		if err != nil {
			return nil, fmt.Errorf("contents src %q: %w", src, err)
		}
		emit(got)
	}

	// scripts.{preinstall,postinstall,preremove,postremove}
	for _, name := range scriptKeys {
		s, ok := lookupString(scriptsMap(root), name)
		if !ok {
			continue
		}
		got, err := resolve(realPkgDir, s)
		if err != nil {
			return nil, fmt.Errorf("scripts.%s %q: %w", name, s, err)
		}
		emit(got)
	}

	return refs, nil
}

// resolve classifies one path and returns the Refs it produces. An empty
// return slice with a nil error means the path is passthrough (absolute
// or ${VAR}-prefixed) and contributes no aux file.
func resolve(realPkgDir, src string) ([]Ref, error) {
	if src == "" {
		return nil, fmt.Errorf("empty path")
	}
	if strings.ContainsRune(src, 0) {
		return nil, fmt.Errorf("contains NUL byte")
	}
	if err := checkPathComponents(src); err != nil {
		return nil, err
	}

	// Passthrough shapes — nothing to inline.
	if strings.HasPrefix(src, "/") || strings.HasPrefix(src, "${") {
		return nil, nil
	}

	if hasGlobMeta(src) {
		return resolveGlob(realPkgDir, src)
	}
	return resolveLiteralOrDir(realPkgDir, src)
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// resolveGlob expands a glob against realPkgDir using the same matcher
// nfpm uses internally. Each match is checked for containment, .tmpl
// rejection, and path-component sanity.
func resolveGlob(realPkgDir, src string) ([]Ref, error) {
	pattern := path.Clean(filepath.ToSlash(strings.TrimPrefix(src, "./")))
	if pattern == "." || pattern == "/" {
		return nil, fmt.Errorf("glob resolves to package root")
	}
	fsys := os.DirFS(realPkgDir)
	matches, err := doublestar.Glob(fsys, pattern)
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("matched no files")
	}

	var out []Ref
	for _, m := range matches {
		// fs match is slash-separated and rooted at realPkgDir.
		full := filepath.Join(realPkgDir, filepath.FromSlash(m))
		info, err := os.Lstat(full)
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", m, err)
		}
		if info.IsDir() {
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", m)
		}
		if strings.HasSuffix(m, ".tmpl") {
			return nil, fmt.Errorf("match %s ends in .tmpl (cooper will not silently ship an unrendered template)", m)
		}
		real, err := containedReal(realPkgDir, full)
		if err != nil {
			return nil, err
		}
		out = append(out, Ref{
			Key:     "./" + m,
			AbsPath: real,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("matched only directories")
	}
	return out, nil
}

// resolveLiteralOrDir handles a path with no glob metacharacters. The
// path may resolve to a directory (recursive walk), a regular file
// (literal-file rule), or neither — in which case "<P>.tmpl" is checked
// for the .tmpl-fallback rule.
//
// We use os.Stat (not Lstat) so symlinks pointing inside the package dir
// follow their target; containedReal then catches symlinks pointing
// outside via the EvalSymlinks + Rel containment check.
func resolveLiteralOrDir(realPkgDir, src string) ([]Ref, error) {
	abs := filepath.Join(realPkgDir, filepath.FromSlash(strings.TrimPrefix(src, "./")))
	info, err := os.Stat(abs)

	if err == nil && info.IsDir() {
		return resolveDirWalk(realPkgDir, abs)
	}

	bareExists := err == nil && info.Mode().IsRegular()
	if err == nil && !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", src)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", src, err)
	}

	tmplPath := abs + ".tmpl"
	tmplInfo, tmplErr := os.Stat(tmplPath)
	tmplExists := tmplErr == nil && tmplInfo.Mode().IsRegular()
	if tmplErr != nil && !errors.Is(tmplErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", src+".tmpl", tmplErr)
	}

	switch {
	case bareExists && tmplExists:
		return nil, fmt.Errorf("both %s and %s.tmpl exist; delete one", src, src)
	case bareExists:
		real, err := containedReal(realPkgDir, abs)
		if err != nil {
			return nil, err
		}
		return []Ref{{Key: src, AbsPath: real}}, nil
	case tmplExists:
		real, err := containedReal(realPkgDir, tmplPath)
		if err != nil {
			return nil, err
		}
		return []Ref{{Key: src, AbsPath: real, Template: true}}, nil
	default:
		return nil, fmt.Errorf("neither %s nor %s.tmpl exists", src, src)
	}
}

// resolveDirWalk recursively stages every regular file under absDir.
// Symlinks, devices, FIFOs, and .tmpl files are hard errors per
// cooper-design.md §"Aux file resolution" and §"Security & sandboxing".
func resolveDirWalk(realPkgDir, absDir string) ([]Ref, error) {
	var out []Ref
	err := filepath.WalkDir(absDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		if strings.HasSuffix(p, ".tmpl") {
			return fmt.Errorf("directory walk surfaced %s (cooper will not silently ship an unrendered template)", p)
		}
		real, err := containedReal(realPkgDir, p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(realPkgDir, real)
		if err != nil {
			return err
		}
		if err := checkPathComponents(rel); err != nil {
			return err
		}
		out = append(out, Ref{
			Key:     "./" + filepath.ToSlash(rel),
			AbsPath: real,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// containedReal resolves p through symlinks and asserts the result stays
// inside realPkgDir.
func containedReal(realPkgDir, p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	rel, err := filepath.Rel(realPkgDir, real)
	if err != nil {
		return "", fmt.Errorf("relativize %s: %w", real, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside package directory", p)
	}
	return real, nil
}

func checkPathComponents(p string) error {
	for comp := range strings.SplitSeq(filepath.ToSlash(p), "/") {
		if len(comp) > maxPathComponentBytes {
			return fmt.Errorf("path component longer than %d bytes", maxPathComponentBytes)
		}
	}
	return nil
}
