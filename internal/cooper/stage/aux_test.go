package stage

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parseDoc parses a YAML mapping into a yaml.Node so tests can construct
// nfpm doc 2 inline.
func parseDoc(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(body), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

// makePkg writes the named files (mapping path → contents) under a fresh
// tempdir and returns the dir.
func makePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func keysOf(refs []Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Key
	}
	sort.Strings(out)
	return out
}

func TestWalk_LiteralFile(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service": "[Unit]\n",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /lib/systemd/system/hugo.service }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs: %v", refs)
	}
	if refs[0].Key != "./hugo.service" {
		t.Errorf("key: %s", refs[0].Key)
	}
	if refs[0].Template {
		t.Errorf("expected non-template ref")
	}
}

func TestWalk_TemplateFallback(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service.tmpl": "Version={{.Version}}\n",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /lib/systemd/system/hugo.service }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || !refs[0].Template {
		t.Fatalf("expected one template ref: %+v", refs)
	}
	if refs[0].Key != "./hugo.service" {
		t.Errorf("key should be the user's spelling, got %s", refs[0].Key)
	}
	if !strings.HasSuffix(refs[0].AbsPath, ".tmpl") {
		t.Errorf("AbsPath should point at the .tmpl: %s", refs[0].AbsPath)
	}
}

func TestWalk_TmplCollision(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service":      "x",
		"hugo.service.tmpl": "y",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /lib/systemd/system/hugo.service }`)
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), "delete one") {
		t.Fatalf("expected collision error, got %v", err)
	}
}

func TestWalk_NeitherExists(t *testing.T) {
	dir := makePkg(t, nil)
	doc := parseDoc(t, `contents:
  - { src: ./missing.service, dst: /x }`)
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("expected neither-exists error, got %v", err)
	}
}

func TestWalk_GlobMultipleMatches(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"completions/hugo.bash": "b",
		"completions/hugo.zsh":  "z",
		"completions/hugo.fish": "f",
	})
	doc := parseDoc(t, `contents:
  - { src: ./completions/*, dst: /usr/share/bash-completion/completions/ }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("refs: %d", len(refs))
	}
	want := []string{
		"./completions/hugo.bash",
		"./completions/hugo.fish",
		"./completions/hugo.zsh",
	}
	got := keysOf(refs)
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %s want %s", i, got[i], w)
		}
	}
}

func TestWalk_GlobZeroMatches(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"unrelated.txt": "x",
	})
	doc := parseDoc(t, `contents:
  - { src: ./bin/*, dst: /usr/bin/ }`)
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("expected zero-match error, got %v", err)
	}
}

func TestWalk_GlobMatchesTmplRejected(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"profiles/foo.tmpl": "x",
	})
	doc := parseDoc(t, `contents:
  - { src: ./profiles/*, dst: /etc/foo/ }`)
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), ".tmpl") {
		t.Fatalf("expected .tmpl rejection, got %v", err)
	}
}

func TestWalk_DirectoryRecursive(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"share/man/man1/hugo.1": "manpage",
		"share/man/man8/hugo.8": "manpage8",
		"share/notes.txt":       "notes",
	})
	doc := parseDoc(t, `contents:
  - { src: ./share, dst: /usr/share }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"./share/man/man1/hugo.1",
		"./share/man/man8/hugo.8",
		"./share/notes.txt",
	}
	if got := keysOf(refs); !equalStrings(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestWalk_DirectoryHittingTmplRejected(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"share/foo":      "raw",
		"share/foo.tmpl": "templated",
	})
	doc := parseDoc(t, `contents:
  - { src: ./share, dst: /usr/share }`)
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), ".tmpl") {
		t.Fatalf("expected .tmpl error, got %v", err)
	}
}

func TestWalk_AbsolutePassthrough(t *testing.T) {
	dir := makePkg(t, nil)
	doc := parseDoc(t, `contents:
  - { src: /usr/share/zoneinfo/UTC, dst: /etc/localtime }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no refs (passthrough), got %v", refs)
	}
}

func TestWalk_AssetsVarPassthrough(t *testing.T) {
	dir := makePkg(t, nil)
	doc := parseDoc(t, `contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no refs (passthrough), got %v", refs)
	}
}

func TestWalk_TraversalEscape(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "pkg")
	if err := os.Mkdir(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := parseDoc(t, `contents:
  - { src: ../outside.txt, dst: /etc/x }`)
	_, err := Walk(pkg, doc)
	if err == nil || !strings.Contains(err.Error(), "outside package directory") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestWalk_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "pkg")
	if err := os.Mkdir(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(pkg, "leak")); err != nil {
		t.Fatal(err)
	}
	doc := parseDoc(t, `contents:
  - { src: ./leak, dst: /etc/x }`)
	_, err := Walk(pkg, doc)
	if err == nil || !strings.Contains(err.Error(), "outside package directory") {
		t.Fatalf("expected symlink-escape rejection, got %v", err)
	}
}

func TestWalk_ScriptsResolved(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo-postinstall.sh": "#!/bin/sh\n",
	})
	doc := parseDoc(t, `contents: []
scripts:
  postinstall: ./hugo-postinstall.sh`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Key != "./hugo-postinstall.sh" {
		t.Errorf("scripts ref: %+v", refs)
	}
}

func TestWalk_DedupesByKey(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"shared.svc": "x",
	})
	doc := parseDoc(t, `contents:
  - { src: ./shared.svc, dst: /a }
  - { src: ./shared.svc, dst: /b }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Errorf("expected 1 deduped ref, got %d", len(refs))
	}
}

func TestWalk_NulInPath(t *testing.T) {
	dir := makePkg(t, nil)
	doc := parseDoc(t, "contents:\n  - { src: \"./foo\\u0000bar\", dst: /x }")
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("expected NUL rejection, got %v", err)
	}
}

func TestWalk_LongComponent(t *testing.T) {
	long := strings.Repeat("a", 256)
	dir := makePkg(t, nil)
	doc := parseDoc(t, "contents:\n  - { src: ./"+long+", dst: /x }")
	_, err := Walk(dir, doc)
	if err == nil || !strings.Contains(err.Error(), "256") && !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("expected long-component rejection, got %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
