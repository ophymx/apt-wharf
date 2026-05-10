package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleSidecarLatest = `---
source:
  github:
    repo: gohugoio/hugo
    release: latest
version_from: tag_strip_v
arches:
  amd64:
    asset: "hugo_${VERSION}_linux-amd64.tar.gz"
---
name: hugo
version: ${VERSION}
arch: ${ARCH}
maintainer: Ophymx <ops@ophymx.com>
description: Static site generator
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
  - src: ./hugo.service
    dst: /lib/systemd/system/hugo.service
scripts:
  postinstall: ./hugo-postinstall.sh
`

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureRepo lays out a minimal cooper.yaml + one package on disk.
func fixtureRepo(t *testing.T, packageBody string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "packages/hugo.yaml"), packageBody)
	for rel, body := range files {
		writeFile(t, filepath.Join(dir, "packages", rel), body)
	}
	writeFile(t, filepath.Join(dir, "cooper.yaml"),
		"packages:\n  - ./packages/hugo.yaml\n")
	return filepath.Join(dir, "cooper.yaml")
}

func TestCmdValidate_Happy(t *testing.T) {
	cfg := fixtureRepo(t, sampleSidecarLatest, map[string]string{
		"hugo.service":         "[Unit]\n",
		"hugo-postinstall.sh":  "#!/bin/sh\n",
	})
	if err := cmdValidate([]string{cfg}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestCmdValidate_TemplateRendersAtValidate(t *testing.T) {
	// .tmpl exists, bare doesn't — validate must render against placeholder Vars.
	cfg := fixtureRepo(t, sampleSidecarLatest, map[string]string{
		"hugo.service.tmpl":   "Version={{ .Version }} Arch={{ .Arch }}\n",
		"hugo-postinstall.sh": "#!/bin/sh\n",
	})
	if err := cmdValidate([]string{cfg}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestCmdValidate_BadTemplateField(t *testing.T) {
	cfg := fixtureRepo(t, sampleSidecarLatest, map[string]string{
		"hugo.service.tmpl":   "Version={{ .Versoin }}\n",
		"hugo-postinstall.sh": "#!/bin/sh\n",
	})
	err := cmdValidate([]string{cfg})
	if err == nil {
		t.Fatal("expected validate failure on template typo")
	}
	if !strings.Contains(err.Error(), "failed validation") {
		t.Errorf("error: %v", err)
	}
}

func TestCmdValidate_MissingAuxFile(t *testing.T) {
	cfg := fixtureRepo(t, sampleSidecarLatest, map[string]string{
		// hugo-postinstall.sh missing
		"hugo.service": "[Unit]\n",
	})
	err := cmdValidate([]string{cfg})
	if err == nil {
		t.Fatal("expected validate failure on missing aux file")
	}
}

func TestCmdValidate_BadConfig(t *testing.T) {
	cfg := fixtureRepo(t, "name: hugo\n", nil) // single doc — invalid
	err := cmdValidate([]string{cfg})
	if err == nil {
		t.Fatal("expected validate failure on malformed package file")
	}
}

func TestCmdValidate_TopMissingPackages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cooper.yaml"), "github: { token_env: GITHUB_TOKEN }\n")
	err := cmdValidate([]string{filepath.Join(dir, "cooper.yaml")})
	if err == nil || !strings.Contains(err.Error(), "load top config") {
		t.Fatalf("expected top-config failure, got %v", err)
	}
}

func TestCmdValidate_NeedsExactlyOneArg(t *testing.T) {
	if err := cmdValidate(nil); err == nil {
		t.Error("expected error for no args")
	}
	if err := cmdValidate([]string{"a", "b"}); err == nil {
		t.Error("expected error for two args")
	}
}
