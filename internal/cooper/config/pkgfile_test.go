package config

import (
	"strings"
	"testing"
)

const validHugoFile = `---
source:
  github:
    repo: gohugoio/hugo
    release: latest
    include_prerelease: false
version_from: tag_strip_v
arches:
  amd64:
    asset: "hugo_extended_${VERSION}_linux-amd64.tar.gz"
  arm64:
    asset: "hugo_extended_${VERSION}_linux-arm64.tar.gz"
---
name: hugo
version: ${VERSION}
arch: ${ARCH}
maintainer: Ophymx <ops@ophymx.com>
description: Static site generator
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
`

func TestLoadPackage_Happy(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "hugo.yaml", validHugoFile)

	got, err := LoadPackage(p)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if got.NfpmName != "hugo" {
		t.Errorf("NfpmName: %s", got.NfpmName)
	}
	if got.Sidecar.Source.GitHub == nil {
		t.Fatal("Sidecar.Source.GitHub nil")
	}
	if got.Sidecar.Source.GitHub.Repo != "gohugoio/hugo" {
		t.Errorf("repo: %s", got.Sidecar.Source.GitHub.Repo)
	}
	if !got.Sidecar.Source.GitHub.Release.Latest {
		t.Errorf("Release.Latest should be true")
	}
	if got.Sidecar.VersionFrom != VersionFromTagStripV {
		t.Errorf("VersionFrom: %s", got.Sidecar.VersionFrom)
	}
	if len(got.Sidecar.Arches) != 2 {
		t.Errorf("Arches: %v", got.Sidecar.Arches)
	}
}

func TestLoadPackage_TagPatternRelease(t *testing.T) {
	body := strings.Replace(validHugoFile,
		"release: latest",
		`release: { tag_pattern: "^v\\d+\\.\\d+\\.\\d+$" }`, 1)
	dir := t.TempDir()
	p := writeFile(t, dir, "hugo.yaml", body)

	got, err := LoadPackage(p)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	r := got.Sidecar.Source.GitHub.Release
	if r.Latest || r.Tag != "" {
		t.Errorf("expected only tag_pattern set: %+v", r)
	}
	if r.TagPattern == "" {
		t.Error("tag_pattern empty")
	}
	if r.CompiledTagPattern() == nil {
		t.Error("expected compiled tag pattern")
	}
	if !r.CompiledTagPattern().MatchString("v1.2.3") {
		t.Error("compiled tag pattern should match v1.2.3")
	}
}

func TestLoadPackage_DefaultsReleaseToLatest(t *testing.T) {
	body := strings.Replace(validHugoFile,
		"release: latest\n    include_prerelease: false\n",
		"include_prerelease: false\n", 1)
	dir := t.TempDir()
	p := writeFile(t, dir, "hugo.yaml", body)

	got, err := LoadPackage(p)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if !got.Sidecar.Source.GitHub.Release.Latest {
		t.Error("expected Release default to {Latest: true}")
	}
}

func TestLoadPackage_VersionFromAssetFilename(t *testing.T) {
	body := `---
source:
  github:
    repo: foo/bar
version_from: asset_filename
version_regex: '^foo-(?P<ver>\d+\.\d+\.\d+)-linux\.tar\.gz$'
arches:
  amd64:
    asset: "foo-${VERSION}-linux.tar.gz"
---
name: foo
`
	dir := t.TempDir()
	p := writeFile(t, dir, "foo.yaml", body)
	got, err := LoadPackage(p)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if got.Sidecar.VersionFrom != VersionFromAssetFilename {
		t.Error("VersionFrom not preserved")
	}
}

func TestLoadPackage_Errors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "single doc",
			body:    "name: hugo\n",
			wantSub: "expected exactly 2 YAML documents",
		},
		{
			name:    "three docs",
			body:    "---\nfoo: 1\n---\nname: hugo\n---\nbar: 2\n",
			wantSub: "expected exactly 2 YAML documents",
		},
		{
			name: "doc 1 unknown field",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
bogus: 1
arches:
  amd64: { asset: x }
---
name: x
`,
			wantSub: "field bogus not found",
		},
		{
			name: "doc 1 missing source.github",
			body: `---
source: {}
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "source.github",
		},
		{
			name: "repo not owner/name",
			body: `---
source: { github: { repo: justaname } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "must be \"owner/name\"",
		},
		{
			name: "invalid release scalar",
			body: `---
source: { github: { repo: a/b, release: bogus } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: `scalar must be "latest"`,
		},
		{
			name: "release union conflict",
			body: `---
source: { github: { repo: a/b, release: { tag_pattern: '^v', tag: v1 } } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "mutually exclusive",
		},
		{
			name: "release unknown key",
			body: `---
source: { github: { repo: a/b, release: { other: x } } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "unknown key",
		},
		{
			name: "invalid tag_pattern regex",
			body: `---
source: { github: { repo: a/b, release: { tag_pattern: '[unclosed' } } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "tag_pattern",
		},
		{
			name: "version_from missing",
			body: `---
source: { github: { repo: a/b } }
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "version_from: required",
		},
		{
			name: "version_from invalid",
			body: `---
source: { github: { repo: a/b } }
version_from: weird
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "version_from",
		},
		{
			name: "version_regex required for asset_filename",
			body: `---
source: { github: { repo: a/b } }
version_from: asset_filename
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "version_regex: required",
		},
		{
			name: "version_regex invalid",
			body: `---
source: { github: { repo: a/b } }
version_from: asset_filename
version_regex: '[unclosed'
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "version_regex",
		},
		{
			name: "version_template required for fixed",
			body: `---
source: { github: { repo: a/b } }
version_from: fixed
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "version_template: required",
		},
		{
			name: "epoch negative",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
epoch: -1
arches: { amd64: { asset: x } }
---
name: x
`,
			wantSub: "epoch must be ≥ 0",
		},
		{
			name: "arches empty",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches: {}
---
name: x
`,
			wantSub: "at least one entry",
		},
		{
			name: "arches missing asset",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches:
  amd64: {}
---
name: x
`,
			wantSub: "asset: required",
		},
		{
			name: "asset selector with non-VERSION substitution",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches:
  amd64: { asset: "foo-${VERSION}-${ARCH}.tar.gz" }
---
name: x
`,
			wantSub: "only ${VERSION} substitution",
		},
		{
			name: "doc 2 missing name",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches: { amd64: { asset: x } }
---
version: 1
`,
			wantSub: "missing required top-level name:",
		},
		{
			name: "doc 2 not a mapping",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches: { amd64: { asset: x } }
---
- foo
- bar
`,
			wantSub: "mapping at top level",
		},
		{
			name: "doc 2 empty name",
			body: `---
source: { github: { repo: a/b } }
version_from: tag
arches: { amd64: { asset: x } }
---
name: ""
`,
			wantSub: "non-empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeFile(t, dir, "x.yaml", tc.body)
			_, err := LoadPackage(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
