package stage

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustNode(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(body), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func emit(t *testing.T, n *yaml.Node) string {
	t.Helper()
	out, err := yaml.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSubstituteNfpm_BasicValues(t *testing.T) {
	n := mustNode(t, `
name: hugo
version: ${VERSION}
arch: ${ARCH}
description: "Hugo ${VERSION} for ${ARCH}"
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
`)
	SubstituteNfpm(n, "0.140.0", "amd64")
	out := emit(t, n)

	for _, want := range []string{
		"version: 0.140.0",
		"arch: amd64",
		"Hugo 0.140.0 for amd64",
		"src: ${ASSETS}/hugo", // ${ASSETS} preserved
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected substring %q, got:\n%s", want, out)
		}
	}
	for _, bad := range []string{"${VERSION}", "${ARCH}"} {
		if strings.Contains(out, bad) {
			t.Errorf("substitution should have replaced %s, got:\n%s", bad, out)
		}
	}
}

func TestSubstituteNfpm_NumericModesPreserved(t *testing.T) {
	n := mustNode(t, `
contents:
  - src: ${ASSETS}/x
    dst: /usr/bin/x
    file_info:
      mode: 0755
`)
	SubstituteNfpm(n, "1.0", "amd64")
	js, err := NfpmToJSON(n)
	if err != nil {
		t.Fatal(err)
	}
	// 0755 in YAML is octal 493 in decimal — must be a JSON number, not "0755".
	if !strings.Contains(string(js), `"mode":493`) {
		t.Errorf("expected mode:493 in JSON, got %s", js)
	}
}

func TestCloneNfpm_DeepCopy(t *testing.T) {
	src := mustNode(t, `
arch: ${ARCH}
contents:
  - src: ${ASSETS}/${ARCH}
    dst: /usr/bin/x
`)
	clone, err := CloneNfpm(src)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the clone; src must stay intact.
	SubstituteNfpm(clone, "1.0", "amd64")

	srcOut := emit(t, src)
	cloneOut := emit(t, clone)
	if !strings.Contains(srcOut, "${ARCH}") {
		t.Errorf("src lost its ${ARCH}; clone wasn't deep:\n%s", srcOut)
	}
	if strings.Contains(cloneOut, "${ARCH}") {
		t.Errorf("clone substitution didn't apply:\n%s", cloneOut)
	}
}

func TestNfpmToJSON_Shape(t *testing.T) {
	n := mustNode(t, `
name: hugo
version: 0.140.0
arch: amd64
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
deb:
  fields:
    Bugs: https://example.invalid/issues
`)
	js, err := NfpmToJSON(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"name":"hugo"`,
		`"arch":"amd64"`,
		`"src":"${ASSETS}/hugo"`,
		`"dst":"/usr/bin/hugo"`,
		`"deb":`,
		`"fields":`,
		`"Bugs":"https://example.invalid/issues"`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("missing %q in JSON:\n%s", want, js)
		}
	}
}
