// Package version exposes build-time version information shared by every
// apt-wharf binary (signpost, cooper, chandler, drayman, staves).
//
// Goreleaser sets Version/Commit/Date via -ldflags -X. Plain `go build`
// leaves the vars at their defaults; String falls back to
// runtime/debug.ReadBuildInfo VCS settings so the banner still shows
// something useful in that case.
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Build-time vars. Overridden by ldflags in .goreleaser.yaml.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String returns a single-line version banner for `<tool> --version`.
func String(tool string) string {
	v, c, d := Version, Commit, Date
	if c == "" || d == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if c == "" {
						c = s.Value
					}
				case "vcs.time":
					if d == "" {
						d = s.Value
					}
				}
			}
		}
	}
	if len(c) > 12 {
		c = c[:12]
	}
	var extras []string
	if c != "" {
		extras = append(extras, "commit "+c)
	}
	if d != "" {
		extras = append(extras, "built "+d)
	}
	if len(extras) == 0 {
		return fmt.Sprintf("%s %s", tool, v)
	}
	return fmt.Sprintf("%s %s (%s)", tool, v, strings.Join(extras, ", "))
}
