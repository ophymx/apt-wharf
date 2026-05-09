package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads, env-interpolates, decodes (strict), and validates a config file.
// All failures are loud — caller should treat any error as fatal at startup.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	expanded, err := ExpandEnv(raw)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// yaml.v3 returns io.EOF on a single doc; an extra doc would be a layout
	// mistake we want to flag.
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("parse %s: multiple YAML documents not supported", path)
	}
	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyDefaults(c *Config) {
	if c.GitHub.RateLimit.UnauthenticatedPerHour == 0 {
		c.GitHub.RateLimit.UnauthenticatedPerHour = 50
	}
	if c.GitHub.RateLimit.AuthenticatedPerHour == 0 {
		c.GitHub.RateLimit.AuthenticatedPerHour = 4500
	}
	// XDG fallbacks for paths whose typical /var/lib locations require root.
	if c.Signing.KeyFile == "" {
		c.Signing.KeyFile = DefaultKeyFile()
	}
	if c.Paths.StateDir == "" {
		c.Paths.StateDir = DefaultStateDir()
	}
}
