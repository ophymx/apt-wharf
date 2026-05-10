// Package stage classifies the in-scope file references in nfpm doc 2 and
// resolves them against the package directory, so both `cooper discover`
// and `cooper validate` can identify the bytes that need to ship with
// each .deb.
//
// The walker enforces every aux-resolution rule from cooper-design.md:
// path-shape classification, .tmpl fallback, glob expansion, directory
// recursion, .tmpl-collision detection, and containment under the
// package directory after symlink resolution.
package stage
