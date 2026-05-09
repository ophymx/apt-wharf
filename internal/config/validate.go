package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	sourceNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	debianPkgPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9+\-.]+$`)
	githubRepoPattern  = regexp.MustCompile(`^[^/]+/[^/]+$`)
	suiteCodenameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// Validate enforces every startup rule from design-mvp.md. All errors are
// joined so a single run surfaces every problem rather than only the first.
func Validate(c *Config) error {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	collect(validateRepository(&c.Repository))
	collect(validateSuite(&c.Suite))
	collect(validateBootstrap(&c.Bootstrap))
	collect(validateSigning(&c.Signing))
	collect(validateServer(&c.Server))
	collect(validateRefresh(&c.Refresh))
	collect(validateGitHub(&c.GitHub))
	collect(validatePaths(&c.Paths))
	collect(validateSources(c))

	return errors.Join(errs...)
}

func validateRepository(r *Repository) error {
	if r.Origin == "" {
		return fmt.Errorf("repository.origin is required")
	}
	if r.Label == "" {
		return fmt.Errorf("repository.label is required")
	}
	if r.BaseURL == "" {
		return fmt.Errorf("repository.base_url is required")
	}
	u, err := url.Parse(r.BaseURL)
	if err != nil {
		return fmt.Errorf("repository.base_url %q: %w", r.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("repository.base_url %q must be http or https", r.BaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("repository.base_url %q is missing host", r.BaseURL)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("repository.base_url %q must not have a path", r.BaseURL)
	}
	if strings.HasSuffix(r.BaseURL, "/") {
		return fmt.Errorf("repository.base_url %q must not have a trailing slash", r.BaseURL)
	}
	return nil
}

func validateSuite(s *Suite) error {
	if s.Codename == "" {
		return fmt.Errorf("suite.codename is required")
	}
	if !suiteCodenameRegex.MatchString(s.Codename) {
		return fmt.Errorf("suite.codename %q must match %s", s.Codename, suiteCodenameRegex)
	}
	if s.Description == "" {
		return fmt.Errorf("suite.description is required")
	}
	if len(s.Architectures) == 0 {
		return fmt.Errorf("suite.architectures must list at least one architecture")
	}
	seen := make(map[string]struct{}, len(s.Architectures))
	for _, a := range s.Architectures {
		if a == "" {
			return fmt.Errorf("suite.architectures contains an empty entry")
		}
		if a == "all" {
			return fmt.Errorf("suite.architectures must not contain %q (handled implicitly)", a)
		}
		if _, dup := seen[a]; dup {
			return fmt.Errorf("suite.architectures contains duplicate %q", a)
		}
		seen[a] = struct{}{}
	}
	return nil
}

func validateBootstrap(b *Bootstrap) error {
	if !debianPkgPattern.MatchString(b.PackageName) {
		return fmt.Errorf("bootstrap.package_name %q is not a valid Debian package name", b.PackageName)
	}
	if strings.TrimSpace(b.Maintainer) == "" {
		return fmt.Errorf("bootstrap.maintainer is required")
	}
	if strings.TrimSpace(b.Description) == "" {
		return fmt.Errorf("bootstrap.description is required")
	}
	return nil
}

func validateSigning(s *Signing) error {
	if s.KeyFile == "" {
		return fmt.Errorf("signing.key_file is required")
	}
	if !filepath.IsAbs(s.KeyFile) {
		return fmt.Errorf("signing.key_file %q must be absolute", s.KeyFile)
	}
	// When auto_generate is on, the file may not exist yet — defer the
	// secure-file check until after sign.EnsureKey has had a chance to
	// materialize it.
	if !s.AutoGenerate {
		if err := CheckSecureFile(s.KeyFile, "signing.key_file"); err != nil {
			return err
		}
	}
	if s.NextPubkeyFile != "" {
		if !filepath.IsAbs(s.NextPubkeyFile) {
			return fmt.Errorf("signing.next_pubkey_file %q must be absolute", s.NextPubkeyFile)
		}
		// Pubkey files don't need 0400 — they're public. Existence check only.
		// Distinct-fingerprint check happens in internal/sign at load time.
	}
	if s.PassphraseEnv != "" && s.PassphraseFile != "" {
		return fmt.Errorf("signing.passphrase_env and signing.passphrase_file are mutually exclusive")
	}
	if s.PassphraseFile != "" {
		if !filepath.IsAbs(s.PassphraseFile) {
			return fmt.Errorf("signing.passphrase_file %q must be absolute", s.PassphraseFile)
		}
		if err := CheckSecureFile(s.PassphraseFile, "signing.passphrase_file"); err != nil {
			return err
		}
	}
	// Empty-resolves-to-failure is enforced at LoadSecret time (called by
	// internal/sign); we can't read the env here without forcing every
	// `signpost check` invocation to have the env set.
	return nil
}

func validateServer(s *Server) error {
	if s.Listen == "" {
		return fmt.Errorf("server.listen is required (e.g., :8080)")
	}
	return nil
}

func validateRefresh(r *Refresh) error {
	if r.Interval.AsDuration() <= 0 {
		return fmt.Errorf("refresh.interval must be > 0")
	}
	if r.Jitter.AsDuration() < 0 {
		return fmt.Errorf("refresh.jitter must be >= 0")
	}
	if r.HTTPTimeout.AsDuration() <= 0 {
		return fmt.Errorf("refresh.http_timeout must be > 0")
	}
	return nil
}

func validateGitHub(g *GitHub) error {
	if g.TokenEnv != "" && g.TokenFile != "" {
		return fmt.Errorf("github.token_env and github.token_file are mutually exclusive")
	}
	if g.TokenFile != "" {
		if !filepath.IsAbs(g.TokenFile) {
			return fmt.Errorf("github.token_file %q must be absolute", g.TokenFile)
		}
		if err := CheckSecureFile(g.TokenFile, "github.token_file"); err != nil {
			return err
		}
	}
	if g.RateLimit.UnauthenticatedPerHour <= 0 {
		return fmt.Errorf("github.rate_limit.unauthenticated_per_hour must be > 0")
	}
	if g.RateLimit.AuthenticatedPerHour <= 0 {
		return fmt.Errorf("github.rate_limit.authenticated_per_hour must be > 0")
	}
	return nil
}

func validatePaths(p *Paths) error {
	if p.StateDir == "" {
		return fmt.Errorf("paths.state_dir is required")
	}
	if !filepath.IsAbs(p.StateDir) {
		return fmt.Errorf("paths.state_dir %q must be absolute", p.StateDir)
	}
	return nil
}

func validateSources(c *Config) error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("sources must declare at least one entry")
	}
	var errs []error
	for name, src := range c.Sources {
		if !sourceNamePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("source name %q must match %s", name, sourceNamePattern))
			continue
		}
		if src == nil {
			errs = append(errs, fmt.Errorf("source %s: empty configuration", name))
			continue
		}
		if err := validateDiscovery(name, &src.Discovery); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateDiscovery(sourceName string, d *Discovery) error {
	switch d.Type {
	case "github_release":
		return validateGitHubRelease(sourceName, d)
	case "latest_url":
		return validateLatestURL(sourceName, d)
	case "json_url":
		return validateJSONURL(sourceName, d)
	case "external":
		return validateExternal(sourceName, d)
	default:
		return fmt.Errorf("source %s: discovery.type %q is not supported (supported: github_release, latest_url, json_url, external)",
			sourceName, d.Type)
	}
}

func validateGitHubRelease(sourceName string, d *Discovery) error {
	if d.URL != "" || d.TokenPath != "" || d.AssetURL != "" ||
		len(d.Command) != 0 || d.Timeout.AsDuration() != 0 || len(d.Env) != 0 {
		return fmt.Errorf("source %s: discovery.url/token_path/asset_url/command/timeout/env are not valid for type github_release", sourceName)
	}
	if !githubRepoPattern.MatchString(d.Repo) {
		return fmt.Errorf("source %s: discovery.repo %q must match owner/name", sourceName, d.Repo)
	}
	if d.Asset == "" {
		return fmt.Errorf("source %s: discovery.asset is required", sourceName)
	}
	if _, err := regexp.Compile(d.Asset); err != nil {
		return fmt.Errorf("source %s: discovery.asset %q does not compile as Go regex: %w",
			sourceName, d.Asset, err)
	}
	if d.TokenEnv != "" && d.TokenFile != "" {
		return fmt.Errorf("source %s: discovery.token_env and discovery.token_file are mutually exclusive",
			sourceName)
	}
	if d.TokenFile != "" {
		if !filepath.IsAbs(d.TokenFile) {
			return fmt.Errorf("source %s: discovery.token_file %q must be absolute",
				sourceName, d.TokenFile)
		}
		if err := CheckSecureFile(d.TokenFile,
			fmt.Sprintf("source %s: discovery.token_file", sourceName)); err != nil {
			return err
		}
	}
	return nil
}

func validateLatestURL(sourceName string, d *Discovery) error {
	if d.Repo != "" || d.Asset != "" || d.IncludePrerelease ||
		d.TokenEnv != "" || d.TokenFile != "" ||
		d.TokenPath != "" || d.AssetURL != "" ||
		len(d.Command) != 0 || d.Timeout.AsDuration() != 0 || len(d.Env) != 0 {
		return fmt.Errorf(
			"source %s: discovery.repo/asset/include_prerelease/token_env/token_file/token_path/asset_url/command/timeout/env are not valid for type latest_url",
			sourceName)
	}
	if d.URL == "" {
		return fmt.Errorf("source %s: discovery.url is required for type latest_url", sourceName)
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		return fmt.Errorf("source %s: discovery.url %q: %w", sourceName, d.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("source %s: discovery.url %q must be http or https", sourceName, d.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("source %s: discovery.url %q is missing host", sourceName, d.URL)
	}
	return nil
}

func validateJSONURL(sourceName string, d *Discovery) error {
	if d.Repo != "" || d.Asset != "" || d.IncludePrerelease ||
		d.TokenEnv != "" || d.TokenFile != "" ||
		len(d.Command) != 0 || d.Timeout.AsDuration() != 0 || len(d.Env) != 0 {
		return fmt.Errorf(
			"source %s: discovery.repo/asset/include_prerelease/token_env/token_file/command/timeout/env are not valid for type json_url",
			sourceName)
	}
	if d.URL == "" {
		return fmt.Errorf("source %s: discovery.url is required for type json_url (the JSON metadata endpoint)", sourceName)
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		return fmt.Errorf("source %s: discovery.url %q: %w", sourceName, d.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("source %s: discovery.url %q must be http or https", sourceName, d.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("source %s: discovery.url %q is missing host", sourceName, d.URL)
	}
	if d.TokenPath == "" {
		return fmt.Errorf("source %s: discovery.token_path is required for type json_url (gjson path resolving to the change-detection token)", sourceName)
	}
	if d.AssetURL == "" {
		return fmt.Errorf("source %s: discovery.asset_url is required for type json_url (URL template; {token} resolves to token_path's value, {gjson.path} interpolates other JSON fields)", sourceName)
	}
	return nil
}

func validateExternal(sourceName string, d *Discovery) error {
	if d.URL != "" || d.Repo != "" || d.Asset != "" || d.IncludePrerelease ||
		d.TokenEnv != "" || d.TokenFile != "" ||
		d.TokenPath != "" || d.AssetURL != "" {
		return fmt.Errorf(
			"source %s: discovery.url/repo/asset/include_prerelease/token_env/token_file/token_path/asset_url are not valid for type external",
			sourceName)
	}
	if len(d.Command) == 0 {
		return fmt.Errorf("source %s: discovery.command must list at least one entry", sourceName)
	}
	if strings.TrimSpace(d.Command[0]) == "" {
		return fmt.Errorf("source %s: discovery.command[0] must not be empty", sourceName)
	}
	if !filepath.IsAbs(d.Command[0]) {
		return fmt.Errorf(
			"source %s: discovery.command[0] %q must be an absolute path (PATH is unset for external commands)",
			sourceName, d.Command[0])
	}
	info, err := os.Stat(d.Command[0])
	if err != nil {
		return fmt.Errorf("source %s: discovery.command[0] %q: %w", sourceName, d.Command[0], err)
	}
	if info.IsDir() {
		return fmt.Errorf("source %s: discovery.command[0] %q is a directory", sourceName, d.Command[0])
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("source %s: discovery.command[0] %q is not executable", sourceName, d.Command[0])
	}
	if d.Timeout.AsDuration() < 0 {
		return fmt.Errorf("source %s: discovery.timeout must be >= 0 (default 30s when omitted)", sourceName)
	}
	for k := range d.Env {
		if strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("source %s: discovery.env key %q contains '=' or NUL", sourceName, k)
		}
	}
	return nil
}
