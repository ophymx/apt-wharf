package build

import (
	"testing"
)

func TestBuildEnv(t *testing.T) {
	env := BuildEnv("0.140.0", "amd64", "/work/assets", "", 1715240520)

	wants := map[string]string{
		"VERSION":           "0.140.0",
		"ARCH":              "amd64",
		"ASSETS":            "/work/assets",
		"SOURCE_DATE_EPOCH": "1715240520",
		"LC_ALL":            "C",
		"PATH":              "/usr/local/bin:/usr/bin:/bin",
	}
	got := envMap(env)
	if len(got) != len(wants) {
		t.Errorf("env has %d entries, want %d: %v", len(got), len(wants), got)
	}
	for k, v := range wants {
		if got[k] != v {
			t.Errorf("env[%s]: got %q want %q", k, got[k], v)
		}
	}
	if _, ok := got["SOURCE"]; ok {
		t.Errorf("SOURCE should be absent when sourceDir is empty")
	}

	// Defense-in-depth: nothing inherited from the test process should
	// leak through. Reject any of the common-but-undesired ones.
	for _, banned := range []string{"HOME", "USER", "TMPDIR", "GOPATH", "SHELL"} {
		if _, ok := got[banned]; ok {
			t.Errorf("env should not include %s", banned)
		}
	}
}

func TestBuildEnv_WithSource(t *testing.T) {
	env := BuildEnv("0.140.0", "amd64", "/work/assets", "/work/source", 1715240520)
	got := envMap(env)
	if got["SOURCE"] != "/work/source" {
		t.Errorf("SOURCE: %q, want /work/source", got["SOURCE"])
	}
}
