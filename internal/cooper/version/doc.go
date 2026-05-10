// Package version assembles the resolved ${VERSION} string for a
// discovered package per cooper-design.md §"Version selection". The
// available substitutions for version_template are pinned ({tag},
// {tag_strip_v}, {date}, plus named regex groups when version_from is
// asset_filename) and unknown placeholders are hard errors so typos
// surface at discover time. The final string is validated against
// Debian's version grammar.
package version
