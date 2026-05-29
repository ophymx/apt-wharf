package version

import (
	"strings"
	"testing"
)

func TestString_LdflagsAllSet(t *testing.T) {
	defer restore(Version, Commit, Date)
	Version, Commit, Date = "0.1.0", "abcdef1234567890", "2026-05-29T12:00:00Z"

	got := String("cooper")
	want := "cooper 0.1.0 (commit abcdef123456, built 2026-05-29T12:00:00Z)"
	if got != want {
		t.Errorf("String(cooper):\n  got:  %q\n  want: %q", got, want)
	}
}

func TestString_VersionOnly(t *testing.T) {
	defer restore(Version, Commit, Date)
	Version, Commit, Date = "1.2.3", "", ""

	got := String("signpost")
	// runtime/debug fallback may add commit/date when tests run inside a
	// VCS-tracked tree, so assert on the prefix that always holds.
	if !strings.HasPrefix(got, "signpost 1.2.3") {
		t.Errorf("String prefix: got %q, want prefix %q", got, "signpost 1.2.3")
	}
}

func TestString_CommitTruncated(t *testing.T) {
	defer restore(Version, Commit, Date)
	Version, Commit, Date = "0.1.0", "0123456789abcdef0123", "2026-01-01"

	got := String("staves")
	if !strings.Contains(got, "commit 0123456789ab") {
		t.Errorf("commit not truncated to 12 chars: %q", got)
	}
	if strings.Contains(got, "0123456789abcdef") {
		t.Errorf("full-length commit leaked: %q", got)
	}
}

func restore(v, c, d string) {
	Version, Commit, Date = v, c, d
}
