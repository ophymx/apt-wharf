package plan

import (
	"strconv"
	"strings"
)

// SplitDebianRevision splits a Debian version string into its
// upstream-with-epoch and debian-revision components per Policy §5.6.12.
// The debian-revision is everything after the last hyphen; when no
// hyphen is present the entire string is the upstream-version (possibly
// with a leading "<epoch>:").
//
//	"1.0.0"      → ("1.0.0", "")
//	"1.0.0-2"    → ("1.0.0", "2")
//	"1:0.0-3"    → ("1:0.0", "3")
//	"1.0-rc1-2"  → ("1.0-rc1", "2")  // last hyphen wins
//
// SplitDebianRevision is syntactic only; it does not validate the
// upstream-version or revision against Debian's grammar. Pair it with
// CompareVersions when ordering matters.
func SplitDebianRevision(v string) (upstream, revision string) {
	if i := strings.LastIndex(v, "-"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// CompareVersions compares two Debian version strings per Policy §5.6.12,
// returning -1, 0, or +1 for a < b, a == b, a > b. Implements dpkg's
// verrevcmp with tilde-sorts-before-empty, epoch-as-integer-prefix, and
// the mixed lexicographic/numeric pass over upstream-version and
// debian-revision.
//
// Invalid versions (non-integer epoch, garbage characters) are treated as
// if the offending part were absent — the function never errors. Pair
// with the Debian version grammar (cooper's discover layer enforces it)
// at validation boundaries.
func CompareVersions(a, b string) int {
	epA, upA, revA := splitFull(a)
	epB, upB, revB := splitFull(b)
	if epA != epB {
		if epA < epB {
			return -1
		}
		return 1
	}
	if c := verrevcmp(upA, upB); c != 0 {
		return c
	}
	return verrevcmp(revA, revB)
}

// splitFull splits a Debian version into its three components per
// Policy §5.6.12: optional `<epoch>:`, mandatory upstream-version,
// optional `-<debian-revision>`. Epoch defaults to 0 when absent or
// when the prefix before the first colon doesn't parse as a
// non-negative integer.
func splitFull(v string) (epoch int, upstream, revision string) {
	if i := strings.Index(v, ":"); i >= 0 {
		if n, err := strconv.Atoi(v[:i]); err == nil && n >= 0 {
			epoch = n
			v = v[i+1:]
		}
	}
	upstream, revision = SplitDebianRevision(v)
	return
}

// verrevcmp is the mixed lexicographic/numeric comparison for the
// upstream-version and debian-revision components, per dpkg's
// lib/dpkg/version.c::verrevcmp.
//
// Algorithm: alternate between (a) walking a non-digit prefix
// character-by-character with orderChar's class-based ordering — tilde
// before empty, letters before non-letter punctuation — and (b) stripping
// leading zeros and comparing a digit run by length, then lexicographic
// first-difference.
func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		// Non-digit prefix walk.
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			var ca, cb byte
			if i < len(a) {
				ca = a[i]
			}
			if j < len(b) {
				cb = b[j]
			}
			va := orderChar(ca)
			vb := orderChar(cb)
			if va < vb {
				return -1
			}
			if va > vb {
				return 1
			}
			if i < len(a) {
				i++
			}
			if j < len(b) {
				j++
			}
		}
		// Strip leading zeros on each side.
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		// Digit-run comparison: same length wins by first lexicographic
		// difference; different lengths, longer wins.
		firstDiff := 0
		for i < len(a) && isDigit(a[i]) && j < len(b) && isDigit(b[j]) {
			if firstDiff == 0 {
				firstDiff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if firstDiff < 0 {
			return -1
		}
		if firstDiff > 0 {
			return 1
		}
	}
	return 0
}

// orderChar maps a character to dpkg's verrevcmp sort key:
//   - digits and NUL → 0 (digits aren't compared in this phase; NUL means
//     end-of-string and sorts equal to digit-class)
//   - letters → the letter's ASCII value (so letters sort before
//     non-letter punctuation)
//   - tilde → -1 (sorts before everything, including end-of-string)
//   - other non-digit punctuation → ASCII value + 256 (sorts after
//     letters)
func orderChar(c byte) int {
	switch {
	case isDigit(c):
		return 0
	case isLetter(c):
		return int(c)
	case c == '~':
		return -1
	case c == 0:
		return 0
	default:
		return int(c) + 256
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
