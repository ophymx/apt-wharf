package build

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/pkg/plan"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestWriteNfpmYAML(t *testing.T) {
	bp := plan.BuildPlan{
		Nfpm: json.RawMessage(`{"name":"hugo","version":"0.140.0","arch":"amd64","contents":[{"src":"${ASSETS}/hugo","dst":"/usr/bin/hugo"}]}`),
	}
	target := filepath.Join(t.TempDir(), "nfpm.yaml")
	const hash = "sha256:deadbeef0000000000000000000000000000000000000000000000000000beef"
	if err := WriteNfpmYAML(bp, NfpmYAMLOpts{Hash: hash}, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: hugo",
		"version: 0.140.0",
		"arch: amd64",
		"src: ${ASSETS}/hugo", // ${ASSETS} preserved verbatim for nfpm
		"dst: /usr/bin/hugo",
		"X-Cooper-Build-Inputs-Hash: " + hash,
		"version_schema: none", // cooper pins this so nfpm doesn't pad short versions
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in nfpm.yaml:\n%s", want, got)
		}
	}
}

// TestWriteNfpmYAML_VersionSchemaRespectsRecipe verifies that a recipe
// explicitly choosing a version_schema keeps its choice — cooper only
// supplies the "none" default when the recipe is silent.
func TestWriteNfpmYAML_VersionSchemaRespectsRecipe(t *testing.T) {
	bp := plan.BuildPlan{
		Nfpm: json.RawMessage(`{"name":"hugo","version":"1.2.3","version_schema":"semver","arch":"amd64"}`),
	}
	target := filepath.Join(t.TempDir(), "nfpm.yaml")
	if err := WriteNfpmYAML(bp, NfpmYAMLOpts{}, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "version_schema: semver") {
		t.Errorf("explicit recipe version_schema was overridden:\n%s", got)
	}
	if strings.Contains(string(got), "version_schema: none") {
		t.Errorf("cooper overrode the recipe's explicit version_schema:\n%s", got)
	}
}

func TestWriteNfpmYAML_PreservesExistingDebFields(t *testing.T) {
	bp := plan.BuildPlan{
		Nfpm: json.RawMessage(`{"name":"foo","version":"1.0","deb":{"fields":{"Bugs":"https://example.com/issues"}}}`),
	}
	target := filepath.Join(t.TempDir(), "nfpm.yaml")
	const hash = "sha256:abc"
	if err := WriteNfpmYAML(bp, NfpmYAMLOpts{Hash: hash}, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Bugs: https://example.com/issues",
		"X-Cooper-Build-Inputs-Hash: " + hash,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in nfpm.yaml:\n%s", want, got)
		}
	}
}

func TestWriteAuxFiles(t *testing.T) {
	staging := t.TempDir()
	aux := map[string]plan.AuxFile{
		"./hugo.service":          {ContentB64: b64("[Unit]\nDescription=Hugo\n")},
		"./completions/hugo.bash": {ContentB64: b64("# bash completion\n")},
		"./completions/hugo.zsh":  {ContentB64: b64("# zsh completion\n")},
	}
	if err := WriteAuxFiles(aux, staging); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"hugo.service":          "[Unit]\nDescription=Hugo\n",
		"completions/hugo.bash": "# bash completion\n",
		"completions/hugo.zsh":  "# zsh completion\n",
	} {
		got, err := os.ReadFile(filepath.Join(staging, rel))
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s body: %q", rel, got)
		}
	}
}

func TestWriteAuxFiles_RejectsTraversalKey(t *testing.T) {
	aux := map[string]plan.AuxFile{
		"../escape.txt": {ContentB64: b64("x")},
	}
	err := WriteAuxFiles(aux, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside staging dir") {
		t.Fatalf("expected escape error, got %v", err)
	}
}

func TestPinMtimes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	const epoch int64 = 1715240520 // 2026-05-09T08:12:00Z
	if err := PinMtimes(dir, epoch); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(epoch, 0).UTC()
	for _, rel := range []string{"a", "sub/b"} {
		st, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !st.ModTime().Equal(want) {
			t.Errorf("%s mtime: got %s want %s", rel, st.ModTime(), want)
		}
	}
}

func TestRejectNonZeroOwners_OK(t *testing.T) {
	cases := []string{
		`{"contents":[{"src":"a","dst":"/a"}]}`,                          // no file_info
		`{"contents":[{"src":"a","dst":"/a","file_info":{"mode":493}}]}`, // mode only
		`{"contents":[{"src":"a","dst":"/a","file_info":{"owner":"","group":""}}]}`,
		`{"contents":[{"src":"a","dst":"/a","file_info":{"owner":"root","group":"root"}}]}`,
		`{"contents":[{"src":"a","dst":"/a","file_info":{"owner":"0","group":0}}]}`,
	}
	for _, body := range cases {
		bp := plan.BuildPlan{Nfpm: json.RawMessage(body)}
		if err := RejectNonZeroOwners(bp); err != nil {
			t.Errorf("expected ok for %s: %v", body, err)
		}
	}
}

func TestRejectNonZeroOwners_Rejects(t *testing.T) {
	cases := []struct {
		body, want string
	}{
		{`{"contents":[{"src":"a","dst":"/a","file_info":{"owner":"hugo"}}]}`, "owner = hugo"},
		{`{"contents":[{"src":"a","dst":"/a","file_info":{"group":1000}}]}`, "group = 1000"},
		{`{"contents":[{"src":"a","dst":"/a","file_info":{"owner":"www-data"}}]}`, "owner = www-data"},
	}
	for _, tc := range cases {
		bp := plan.BuildPlan{Nfpm: json.RawMessage(tc.body)}
		err := RejectNonZeroOwners(bp)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("body %s: expected error containing %q, got %v", tc.body, tc.want, err)
		}
	}
}
