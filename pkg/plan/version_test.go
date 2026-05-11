package plan

import (
	"os/exec"
	"sort"
	"testing"
)

func TestSplitDebianRevision(t *testing.T) {
	cases := []struct {
		in           string
		wantUpstream string
		wantRevision string
	}{
		// Bare upstream (no revision).
		{"1.0.0", "1.0.0", ""},
		{"0.140.0", "0.140.0", ""},
		{"2026.05.10", "2026.05.10", ""},

		// Plain debian-revision.
		{"1.0.0-1", "1.0.0", "1"},
		{"1.0.0-2", "1.0.0", "2"},
		{"1.0.0-10", "1.0.0", "10"},

		// Epoch.
		{"1:0.0", "1:0.0", ""},
		{"1:0.0-3", "1:0.0", "3"},
		{"2:1.0.0-1", "2:1.0.0", "1"},

		// Last hyphen wins (upstream may contain hyphens when a
		// debian-revision is present, per Policy §5.6.12).
		{"1.0-rc1-2", "1.0-rc1", "2"},
		{"1.0-rc1-2-3", "1.0-rc1-2", "3"},

		// Revisions with the policy's allowed punctuation.
		{"1.0.0-1+deb12u1", "1.0.0", "1+deb12u1"},
		{"1.0.0-1.2", "1.0.0", "1.2"},
		{"1.0.0-1~bpo12+1", "1.0.0", "1~bpo12+1"},

		// Edge cases — syntactic split is loose by design.
		{"", "", ""},
		{"-", "", ""},
		{"foo-", "foo", ""},
		{"-foo", "", "foo"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotU, gotR := SplitDebianRevision(tc.in)
			if gotU != tc.wantUpstream || gotR != tc.wantRevision {
				t.Errorf("SplitDebianRevision(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotU, gotR, tc.wantUpstream, tc.wantRevision)
			}
		})
	}
}

// TestCompareVersions exercises the corpus pinned in cooper-design.md
// (Policy §5.6.12 worked examples) plus dpkg's own historically-tripping
// edge cases.
func TestCompareVersions(t *testing.T) {
	// Each case asserts CompareVersions(a, b) == want. Symmetry
	// (b, a) == -want and reflexivity (a, a) == 0 are checked
	// automatically below.
	cases := []struct {
		a, b string
		want int
	}{
		// Reflexivity sanity.
		{"1.0.0", "1.0.0", 0},
		{"", "", 0},
		{"1:2.3-4", "1:2.3-4", 0},

		// Plain numeric upstream.
		{"1.0.0", "1.0.1", -1},
		{"1.0.0", "1.1.0", -1},
		{"1.0", "1.0.0", -1},
		{"2.6-1", "2.10-1", -1}, // Policy worked example: numeric, not lexicographic
		{"2.6", "2.10", -1},
		{"0:0.9", "0:0.10", -1},
		{"0.10", "0.9", 1},

		// Tilde sorts before empty / everything.
		{"1.0~beta1", "1.0", -1}, // Policy worked example
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1~git1", "1.0~rc1", -1},
		{"1.0~", "1.0", -1},
		{"~", "", -1},
		{"1.0~1", "1.0", -1},

		// Letters vs end-of-string (letter > empty).
		{"1.0a", "1.0", 1},
		{"1.0a1", "1.0", 1},

		// Letters vs digits at same position — letter < digit in dpkg
		// order ONLY in the non-digit-prefix phase, where digit/end
		// both have order 0 and letters have order(c) > 0. So a letter
		// position sorts AFTER end-of-string (correct: "1.0a" > "1.0")
		// but BEFORE punctuation (a < ~ is false since ~ is -1).
		// Confirmed in this corpus by the entries above.

		// Epoch.
		{"1:0.0", "0.0", 1}, // Policy worked example
		{"1:0.0", "2.0", 1}, // epoch dominates even a much-newer upstream
		{"1:0.0", "1:0.0", 0},
		{"1:0.0", "2:0.0", -1},
		{"0:1.0", "1.0", 0}, // 0: equals no epoch

		// Debian revision.
		{"1.0.0-1", "1.0.0-2", -1},
		{"1.0.0-10", "1.0.0-2", 1}, // 10 > 2 numerically
		{"1.0.0", "1.0.0-1", -1},   // bare < -1 in revision comparison
		{"1.0.0-1", "1.0.0-1+deb12u1", -1},
		{"1.0.0-1~bpo12+1", "1.0.0-1", -1}, // tilde-in-revision still works

		// Long alphanumeric runs (historically tripped reimplementations).
		{"1.0a", "1.0b", -1},
		{"a", "b", -1},
		{"alpha", "beta", -1},
		{"1.0~alpha1", "1.0~beta1", -1},

		// Empty edge cases.
		{"1", "", 1},
		{"", "1", -1},
	}
	for _, tc := range cases {
		name := tc.a + " vs " + tc.b
		t.Run(name, func(t *testing.T) {
			got := CompareVersions(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d",
					tc.a, tc.b, got, tc.want)
			}
			// Symmetry: swapping inputs negates the result.
			gotSym := CompareVersions(tc.b, tc.a)
			if gotSym != -tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d (symmetry)",
					tc.b, tc.a, gotSym, -tc.want)
			}
			// Reflexivity: a == a always.
			if got := CompareVersions(tc.a, tc.a); got != 0 {
				t.Errorf("CompareVersions(%q, %q) = %d, want 0 (reflexivity)",
					tc.a, tc.a, got)
			}
		})
	}
}

// TestCompareVersions_Sort exercises a realistic orchestrator use case:
// finding the highest existing revision per (Package, base-version,
// Architecture). The fixture is a list of versions an orchestrator might
// pull from aptly for a single package; the test asserts the max picks
// the expected entry and that sort order matches dpkg.
func TestCompareVersions_Sort(t *testing.T) {
	versions := []string{
		"1.0.0-2",
		"1.0.0",
		"1.0.0-10",
		"1.0.0-3",
		"1.0.0-1",
	}
	sort.SliceStable(versions, func(i, j int) bool {
		return CompareVersions(versions[i], versions[j]) < 0
	})
	want := []string{
		"1.0.0", // bare = no revision
		"1.0.0-1",
		"1.0.0-2",
		"1.0.0-3",
		"1.0.0-10", // 10 > 3 numerically
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Errorf("sorted[%d] = %q, want %q", i, versions[i], want[i])
		}
	}
}

// TestCompareVersions_DpkgOracle cross-checks CompareVersions against the
// system dpkg --compare-versions for every pair in the in-suite corpus.
// Skipped automatically when dpkg is unavailable (non-Debian hosts, CI
// without it).
func TestCompareVersions_DpkgOracle(t *testing.T) {
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skip("dpkg not on PATH; skipping oracle cross-check")
	}
	versions := []string{
		"", "1", "1.0", "1.0.0", "1.0.0-1", "1.0.0-2", "1.0.0-10",
		"1.0", "1.0a", "1.0~rc1", "1.0~beta1", "1.0~rc1~git1",
		"2.6-1", "2.10-1", "0.9", "0.10",
		"1:0.0", "2:0.0", "0:1.0",
		"1.0.0-1~bpo12+1", "1.0.0-1+deb12u1",
	}
	for _, a := range versions {
		for _, b := range versions {
			want := dpkgCompare(t, a, b)
			got := CompareVersions(a, b)
			if got != want {
				t.Errorf("CompareVersions(%q, %q) = %d, dpkg = %d", a, b, got, want)
			}
		}
	}
}

// dpkgCompare invokes `dpkg --compare-versions a OP b` for each of the
// three operators and returns -1, 0, or +1 to match CompareVersions.
// Treats empty strings as the "0:0" / nothing-version dpkg accepts.
func dpkgCompare(t *testing.T, a, b string) int {
	t.Helper()
	// dpkg refuses an empty string; substitute a value that compares
	// equivalently. "" sorts strictly less than any non-empty version
	// per verrevcmp, and equal to "0:0" per dpkg's normalization. Use
	// "0:0" as the placeholder; CompareVersions("", "0:0") == 0 by the
	// same logic.
	norm := func(s string) string {
		if s == "" {
			return "0:0"
		}
		return s
	}
	an, bn := norm(a), norm(b)
	check := func(op string) bool {
		return exec.Command("dpkg", "--compare-versions", an, op, bn).Run() == nil
	}
	switch {
	case check("eq"):
		return 0
	case check("lt"):
		return -1
	case check("gt"):
		return 1
	default:
		t.Fatalf("dpkg returned no decisive comparison for %q vs %q", an, bn)
		return 0
	}
}
