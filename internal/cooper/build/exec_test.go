package build

import (
	"strings"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	env := BuildEnv("0.140.0", "amd64", "/work/assets", 1715240520)

	wants := map[string]string{
		"VERSION":           "0.140.0",
		"ARCH":              "amd64",
		"ASSETS":            "/work/assets",
		"SOURCE_DATE_EPOCH": "1715240520",
		"LC_ALL":            "C",
		"PATH":              "/usr/local/bin:/usr/bin:/bin",
	}
	got := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			t.Errorf("bad env entry %q", e)
			continue
		}
		got[k] = v
	}
	if len(got) != len(wants) {
		t.Errorf("env has %d entries, want %d: %v", len(got), len(wants), got)
	}
	for k, v := range wants {
		if got[k] != v {
			t.Errorf("env[%s]: got %q want %q", k, got[k], v)
		}
	}

	// Defense-in-depth: nothing inherited from the test process should
	// leak through. Reject any of the common-but-undesired ones.
	for _, banned := range []string{"HOME", "USER", "TMPDIR", "GOPATH", "SHELL"} {
		if _, ok := got[banned]; ok {
			t.Errorf("env should not include %s", banned)
		}
	}
}
