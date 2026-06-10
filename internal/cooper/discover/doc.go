// Package discover orchestrates `cooper discover` per cooper-design.md
// §"Phases · Discover": loads each per-package multi-doc file, resolves
// the GitHub release once per package, then for each arch matches the
// release asset, substitutes ${VERSION}/${ARCH}/${ARCH_GNU} and any per-arch
// arches[].vars keys into doc 2, materializes aux files, and computes
// build_inputs_hash. Per-package failures surface as result:"error"
// entries — they do not abort the run.
package discover
