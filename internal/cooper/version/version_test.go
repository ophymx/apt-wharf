package version

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v86/github"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
)

func ts(t *testing.T, s string) github.Timestamp {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return github.Timestamp{Time: v}
}

//go:fix inline
func sp(s string) *string { return new(s) }

func ghRelease(t *testing.T, tag, publishedAt string, assetNames ...string) ReleaseInfo {
	t.Helper()
	r := &github.RepositoryRelease{TagName: new(tag)}
	pa := ts(t, publishedAt)
	r.PublishedAt = &pa
	for _, n := range assetNames {
		r.Assets = append(r.Assets, &github.ReleaseAsset{Name: new(n)})
	}
	return FromGitHub(r)
}

func mustCompileRelease(t *testing.T, s *config.Sidecar) {
	t.Helper()
	if s.Source.GitHub != nil && s.Source.GitHub.Release != nil && s.Source.GitHub.Release.TagPattern != "" {
		// noop; tests construct sidecars directly so no compile-cache is needed
	}
}

func TestAssemble_Tag(t *testing.T) {
	s := &config.Sidecar{VersionFrom: config.VersionFromTag}
	mustCompileRelease(t, s)
	got, err := Assemble(s, ghRelease(t, "1.2.3", "2026-05-09T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_TagStripV(t *testing.T) {
	s := &config.Sidecar{VersionFrom: config.VersionFromTagStripV}
	got, err := Assemble(s, ghRelease(t, "v0.140.0", "2026-05-09T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.140.0" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_TagStripV_NoLeadingV(t *testing.T) {
	s := &config.Sidecar{VersionFrom: config.VersionFromTagStripV}
	got, err := Assemble(s, ghRelease(t, "1.2.3", "2026-05-09T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3" {
		t.Errorf("strip_v on a tag without v: got %s", got)
	}
}

func TestAssemble_VersionTemplate(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromTagStripV,
		VersionTemplate: "{tag_strip_v}+ds1",
	}
	got, err := Assemble(s, ghRelease(t, "v1.4.2", "2026-05-09T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.4.2+ds1" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_DateSubstitution(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromTag,
		VersionTemplate: "{tag}~git{date}",
	}
	got, err := Assemble(s, ghRelease(t, "1.0.0", "2026-05-09T08:12:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.0.0~git20260509" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_Fixed(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromFixed,
		VersionTemplate: "0~unreleased",
	}
	got, err := Assemble(s, ghRelease(t, "weird-tag", "2026-05-09T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "0~unreleased" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_Fixed_RequiresTemplate(t *testing.T) {
	s := &config.Sidecar{VersionFrom: config.VersionFromFixed}
	_, err := Assemble(s, ghRelease(t, "x", "2026-05-09T08:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "fixed requires version_template") {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestAssemble_AssetFilename_FirstCapture(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:  config.VersionFromAssetFilename,
		VersionRegex: `^foo-(\d+\.\d+\.\d+)-linux-amd64\.tar\.gz$`,
	}
	rel := ghRelease(t, "release-2026-05-09", "2026-05-09T08:00:00Z",
		"foo-1.7.5-linux-amd64.tar.gz",
		"foo-1.7.5-windows-amd64.zip",
	)
	got, err := Assemble(s, rel)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.7.5" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_AssetFilename_NamedGroups(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromAssetFilename,
		VersionRegex:    `^foo-(?P<core>\d+\.\d+\.\d+)-(?P<flavor>[a-z]+)-linux-amd64\.tar\.gz$`,
		VersionTemplate: "{core}+{flavor}",
	}
	rel := ghRelease(t, "x", "2026-05-09T08:00:00Z",
		"foo-1.7.5-stable-linux-amd64.tar.gz",
	)
	got, err := Assemble(s, rel)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.7.5+stable" {
		t.Errorf("got %s", got)
	}
}

func TestAssemble_AssetFilename_NoMatch(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:  config.VersionFromAssetFilename,
		VersionRegex: `^foo-(\d+)\.tar\.gz$`,
	}
	rel := ghRelease(t, "x", "2026-05-09T08:00:00Z",
		"bar-1.0.tar.gz",
	)
	_, err := Assemble(s, rel)
	if err == nil || !strings.Contains(err.Error(), "matched no asset names") {
		t.Fatalf("expected no-match error, got %v", err)
	}
}

func TestAssemble_AssetFilename_NoCaptureGroup(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:  config.VersionFromAssetFilename,
		VersionRegex: `\.tar\.gz$`, // no capture group
	}
	rel := ghRelease(t, "x", "2026-05-09T08:00:00Z", "foo.tar.gz")
	_, err := Assemble(s, rel)
	if err == nil || !strings.Contains(err.Error(), "at least one capture group") {
		t.Fatalf("expected capture-group error, got %v", err)
	}
}

func TestAssemble_UnknownTemplateName(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromTagStripV,
		VersionTemplate: "{tag_strip_v}+{bogus}",
	}
	_, err := Assemble(s, ghRelease(t, "v1.2.3", "2026-05-09T08:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown-substitution error, got %v", err)
	}
}

func TestAssemble_DebianGrammarRejected(t *testing.T) {
	s := &config.Sidecar{VersionFrom: config.VersionFromTag}
	// Tag starts with a letter — not a valid Debian upstream version.
	_, err := Assemble(s, ghRelease(t, "abc-1.0", "2026-05-09T08:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "Debian") {
		t.Fatalf("expected Debian-grammar error, got %v", err)
	}
}

func TestAssemble_DebianGrammarUnderscoreRejected(t *testing.T) {
	s := &config.Sidecar{
		VersionFrom:     config.VersionFromTag,
		VersionTemplate: "{tag}_extra",
	}
	_, err := Assemble(s, ghRelease(t, "1.2.3", "2026-05-09T08:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "Debian") {
		t.Fatalf("expected Debian-grammar error on _, got %v", err)
	}
}
