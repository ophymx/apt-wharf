package stage

import (
	"errors"
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
	if err := SubstituteNfpm(n, "amd64", BuildSubs("0.140.0", "amd64", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	if err := SubstituteNfpm(n, "amd64", BuildSubs("1.0", "amd64", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	if err := SubstituteNfpm(clone, "amd64", BuildSubs("1.0", "amd64", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	srcOut := emit(t, src)
	cloneOut := emit(t, clone)
	if !strings.Contains(srcOut, "${ARCH}") {
		t.Errorf("src lost its ${ARCH}; clone wasn't deep:\n%s", srcOut)
	}
	if strings.Contains(cloneOut, "${ARCH}") {
		t.Errorf("clone substitution didn't apply:\n%s", cloneOut)
	}
}

func TestSubstituteNfpm_ArchGNUResolved(t *testing.T) {
	n := mustNode(t, `
contents:
  - src: ${ASSETS}/${ARCH_GNU}/bin/tool
    dst: /usr/bin/tool
`)
	if err := SubstituteNfpm(n, "amd64", BuildSubs("1.0", "amd64", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := emit(t, n)
	if !strings.Contains(out, "${ASSETS}/x86_64/bin/tool") {
		t.Errorf("ARCH_GNU should have resolved to x86_64, got:\n%s", out)
	}
}

func TestSubstituteNfpm_PerArchVarsResolved(t *testing.T) {
	n := mustNode(t, `
contents:
  - src: ${ASSETS}/${TARBALL_DIR}/bin/tool
    dst: /usr/bin/tool
`)
	subs := BuildSubs("1.0", "amd64", map[string]string{
		"TARBALL_DIR": "x86_64-unknown-linux-gnu",
	})
	if err := SubstituteNfpm(n, "amd64", subs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := emit(t, n)
	if !strings.Contains(out, "${ASSETS}/x86_64-unknown-linux-gnu/bin/tool") {
		t.Errorf("per-arch var should have resolved, got:\n%s", out)
	}
}

func TestSubstituteNfpm_ArchGNUUnknown(t *testing.T) {
	n := mustNode(t, `
contents:
  - src: ${ASSETS}/${ARCH_GNU}/bin/tool
    dst: /usr/bin/tool
  - src: ${ASSETS}/${ARCH_GNU}/share/man
    dst: /usr/share/man
`)
	err := SubstituteNfpm(n, "exotic_arch", BuildSubs("1.0", "exotic_arch", nil))
	if err == nil {
		t.Fatal("expected error for ARCH_GNU on unsupported arch, got nil")
	}
	var gnuErr *ArchGNUUnknownError
	if !errors.As(err, &gnuErr) {
		t.Fatalf("expected *ArchGNUUnknownError, got %T: %v", err, err)
	}
	if gnuErr.Arch != "exotic_arch" {
		t.Errorf("arch mismatch: got %q", gnuErr.Arch)
	}
	if len(gnuErr.Refs) != 2 {
		t.Errorf("expected 2 refs, got %d: %v", len(gnuErr.Refs), gnuErr.Refs)
	}
	msg := err.Error()
	// Mapping table must appear verbatim.
	for _, want := range []string{"amd64", "x86_64", "arm64", "aarch64", "armhf", "armv7l", "riscv64"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
	// Path-style location must appear.
	if !strings.Contains(msg, "contents[0].src") {
		t.Errorf("error message missing path contents[0].src:\n%s", msg)
	}
	// Fix guidance must mention vars.
	if !strings.Contains(msg, "vars") {
		t.Errorf("error message should reference vars fix:\n%s", msg)
	}
}

func TestSubstituteNfpm_UnresolvedKey(t *testing.T) {
	n := mustNode(t, `
contents:
  - src: ${ASSETS}/${MISSING_KEY}/bin/tool
    dst: /usr/bin/tool
`)
	err := SubstituteNfpm(n, "amd64", BuildSubs("1.0", "amd64", nil))
	if err == nil {
		t.Fatal("expected error for unresolved key, got nil")
	}
	var unErr *UnresolvedSubstitutionError
	if !errors.As(err, &unErr) {
		t.Fatalf("expected *UnresolvedSubstitutionError, got %T: %v", err, err)
	}
	if len(unErr.Refs) != 1 || unErr.Refs[0].Key != "MISSING_KEY" {
		t.Errorf("expected one ref for MISSING_KEY, got %+v", unErr.Refs)
	}
	if unErr.Refs[0].Path != "contents[0].src" {
		t.Errorf("expected path contents[0].src, got %q", unErr.Refs[0].Path)
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("error message missing key name:\n%s", err)
	}
}

func TestSubstituteNfpm_UnresolvedAggregatesAcrossPaths(t *testing.T) {
	n := mustNode(t, `
name: ${ALPHA}
contents:
  - src: ${ASSETS}/${BETA}/bin/tool
    dst: /usr/bin/tool
  - src: ${ASSETS}/${ALPHA}/share/${GAMMA}
    dst: /usr/share/tool
`)
	err := SubstituteNfpm(n, "amd64", BuildSubs("1.0", "amd64", nil))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var unErr *UnresolvedSubstitutionError
	if !errors.As(err, &unErr) {
		t.Fatalf("expected *UnresolvedSubstitutionError, got %T", err)
	}
	// Three distinct refs (ALPHA appears twice — once at name, once
	// inside the second contents entry — counted as two distinct
	// occurrences because the paths differ).
	if len(unErr.Refs) != 4 {
		t.Errorf("expected 4 distinct ref occurrences, got %d: %+v", len(unErr.Refs), unErr.Refs)
	}
}

func TestSubstituteNfpm_LowercaseRefsPassthrough(t *testing.T) {
	// Lowercase or mixed-case ${...} sequences don't match cooper's
	// substitution grammar (uppercase-only); they pass through
	// untouched so legitimate embedded text in a description doesn't
	// surprise-fail discover.
	n := mustNode(t, `
description: "Tool that respects ${pwd} and ${Mixed_Case}"
contents:
  - src: ${ASSETS}/tool
    dst: /usr/bin/tool
`)
	if err := SubstituteNfpm(n, "amd64", BuildSubs("1.0", "amd64", nil)); err != nil {
		t.Fatalf("lowercase passthrough should not error: %v", err)
	}
	out := emit(t, n)
	for _, want := range []string{"${pwd}", "${Mixed_Case}"} {
		if !strings.Contains(out, want) {
			t.Errorf("non-conforming %q should have passed through, got:\n%s", want, out)
		}
	}
}

func TestSubstituteNfpm_ScalarTypeIntegrity(t *testing.T) {
	// Non-string scalars (numbers, booleans) must not be touched by
	// substitution — otherwise mode: 0755 would get rewritten as a
	// string and downstream JSON encoding would emit "0755" instead of
	// 493.
	n := mustNode(t, `
release: 1
fips: false
version: "${VERSION}"
`)
	if err := SubstituteNfpm(n, "amd64", BuildSubs("0.1.0", "amd64", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := emit(t, n)
	if !strings.Contains(out, "release: 1") || !strings.Contains(out, "fips: false") {
		t.Errorf("non-string scalars rewritten:\n%s", out)
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
