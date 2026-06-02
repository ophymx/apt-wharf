package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeExecutable creates an empty 0755 file at dir/name and returns its
// path. Used as a stand-in signer binary so the path/mode checks pass.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestValidateSigningExternal_HappyPath(t *testing.T) {
	dir := t.TempDir()
	cmd := makeExecutable(t, dir, "shim.sh")

	s := &Signing{
		External: &SigningExternal{
			Command: []string{cmd},
			Key:     "release-2025",
		},
	}
	if err := validateSigning(s); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateSigningExternal_RejectsInternalFields(t *testing.T) {
	dir := t.TempDir()
	cmd := makeExecutable(t, dir, "shim.sh")

	cases := []struct {
		name string
		mut  func(*Signing)
		want string
	}{
		{"key_file set", func(s *Signing) { s.KeyFile = "/var/lib/signpost/secring.gpg" }, "external mode"},
		{"passphrase_env set", func(s *Signing) { s.PassphraseEnv = "X" }, "external mode"},
		{"passphrase_file set", func(s *Signing) { s.PassphraseFile = "/x" }, "external mode"},
		{"auto_generate set", func(s *Signing) { s.AutoGenerate = true }, "external mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Signing{External: &SigningExternal{Command: []string{cmd}}}
			tc.mut(s)
			err := validateSigning(s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateSigningExternal_CommandChecks(t *testing.T) {
	dir := t.TempDir()
	good := makeExecutable(t, dir, "shim.sh")
	nonExec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(nonExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ext  *SigningExternal
		want string
	}{
		{"empty command", &SigningExternal{}, "command must list"},
		{"relative path", &SigningExternal{Command: []string{"shim.sh"}}, "absolute path"},
		{"missing file", &SigningExternal{Command: []string{filepath.Join(dir, "nope")}}, "no such file"},
		{"directory", &SigningExternal{Command: []string{dir}}, "is a directory"},
		{"non-exec", &SigningExternal{Command: []string{nonExec}}, "not executable"},
		{"bad timeout", &SigningExternal{Command: []string{good}, Timeout: -1}, "timeout must be >= 0"},
		{"env with NUL", &SigningExternal{Command: []string{good}, Env: map[string]string{"a\x00b": "x"}}, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Signing{External: tc.ext}
			err := validateSigning(s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateSigningExternal_PubkeyFile(t *testing.T) {
	dir := t.TempDir()
	cmd := makeExecutable(t, dir, "shim.sh")
	pub := filepath.Join(dir, "release.pub")
	if err := os.WriteFile(pub, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Signing{External: &SigningExternal{
		Command:    []string{cmd},
		PubkeyFile: pub,
	}}
	if err := validateSigning(s); err != nil {
		t.Fatalf("happy path: %v", err)
	}

	s.External.PubkeyFile = "relative.pub"
	if err := validateSigning(s); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want absolute-path error, got %v", err)
	}

	s.External.PubkeyFile = filepath.Join(dir, "missing")
	if err := validateSigning(s); err == nil {
		t.Fatal("want error for missing pubkey_file")
	}

	s.External.PubkeyFile = dir
	if err := validateSigning(s); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want directory error, got %v", err)
	}
}
