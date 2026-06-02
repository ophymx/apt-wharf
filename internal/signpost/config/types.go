package config

import "time"

type Config struct {
	Repository Repository         `yaml:"repository"`
	Suite      Suite              `yaml:"suite"`
	Bootstrap  Bootstrap          `yaml:"bootstrap"`
	Signing    Signing            `yaml:"signing"`
	Server     Server             `yaml:"server"`
	Refresh    Refresh            `yaml:"refresh"`
	GitHub     GitHub             `yaml:"github"`
	Paths      Paths              `yaml:"paths"`
	Sources    map[string]*Source `yaml:"sources"`
}

type Repository struct {
	Origin  string `yaml:"origin"`
	Label   string `yaml:"label"`
	BaseURL string `yaml:"base_url"`
}

type Suite struct {
	Codename      string   `yaml:"codename"`
	Description   string   `yaml:"description"`
	Architectures []string `yaml:"architectures"`
}

type Bootstrap struct {
	PackageName string `yaml:"package_name"`
	Maintainer  string `yaml:"maintainer"`
	Description string `yaml:"description"`
}

type Signing struct {
	KeyFile        string `yaml:"key_file"`
	PassphraseEnv  string `yaml:"passphrase_env"`
	PassphraseFile string `yaml:"passphrase_file"`
	NextPubkeyFile string `yaml:"next_pubkey_file"`

	// AutoGenerate, when true, creates a fresh unencrypted RSA signing key
	// at KeyFile on startup if no file exists there yet. Intended for local
	// dev / first-run UX. The generated key's UID is derived from
	// bootstrap.maintainer.
	AutoGenerate bool `yaml:"auto_generate"`

	// External, when set, delegates signing to an exec'd command instead
	// of the in-process internal/sign path. KeyFile, PassphraseEnv,
	// PassphraseFile, and AutoGenerate are rejected by validation when
	// External is set — the daemon holds no secret material in that mode.
	// NextPubkeyFile is still consulted (operator-supplied rotation pubkey).
	// See internal/sign/external.go for the CLI contract.
	External *SigningExternal `yaml:"external"`
}

// SigningExternal mirrors internal/sign.ExternalConfig at the config layer.
// Field semantics live there; this struct only carries YAML tags + the
// validation rules from internal/config.
type SigningExternal struct {
	Command    []string          `yaml:"command"`
	Key        string            `yaml:"key"`
	PubkeyFile string            `yaml:"pubkey_file"`
	Timeout    Duration          `yaml:"timeout"`
	Env        map[string]string `yaml:"env"`
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Refresh struct {
	Interval    Duration `yaml:"interval"`
	Jitter      Duration `yaml:"jitter"`
	HTTPTimeout Duration `yaml:"http_timeout"`
}

type GitHub struct {
	TokenEnv  string    `yaml:"token_env"`
	TokenFile string    `yaml:"token_file"`
	RateLimit RateLimit `yaml:"rate_limit"`
}

type RateLimit struct {
	UnauthenticatedPerHour int `yaml:"unauthenticated_per_hour"`
	AuthenticatedPerHour   int `yaml:"authenticated_per_hour"`
}

type Paths struct {
	StateDir string `yaml:"state_dir"`
}

type Source struct {
	Enabled   *bool     `yaml:"enabled"`
	Discovery Discovery `yaml:"discovery"`
}

func (s *Source) IsEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// Discovery is a flat-union over the supported discovery types. Type-specific
// fields not relevant to the chosen Type must be unset; validateDiscovery
// enforces that mutual exclusion.
type Discovery struct {
	Type string `yaml:"type"`

	// github_release fields
	Repo              string `yaml:"repo"`
	Asset             string `yaml:"asset"`
	IncludePrerelease bool   `yaml:"include_prerelease"`
	TokenEnv          string `yaml:"token_env"`
	TokenFile         string `yaml:"token_file"`

	// latest_url, json_url, and xml_url share the URL field — the
	// upstream HTTP endpoint to GET. For latest_url it's the asset URL
	// itself; for json_url it's the metadata endpoint that returns JSON;
	// for xml_url it's the metadata endpoint that returns XML.
	URL string `yaml:"url"`

	// json_url + xml_url share asset_url (URL template with
	// {token}/{path} placeholders against the metadata body).
	AssetURL string `yaml:"asset_url"`

	// json_url field — gjson path resolving to the change-detection token.
	TokenPath string `yaml:"token_path"`

	// xml_url field — XPath expression resolving to the change-detection token.
	TokenXPath string `yaml:"token_xpath"`

	// external fields
	Command []string          `yaml:"command"`
	Timeout Duration          `yaml:"timeout"`
	Env     map[string]string `yaml:"env"`
}

type Duration time.Duration

func (d Duration) AsDuration() time.Duration { return time.Duration(d) }
