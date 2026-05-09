package config

import (
	"os"
	"path/filepath"
)

// XDG Base Directory resolution per
// https://specifications.freedesktop.org/basedir-spec/. Used to pick sane
// defaults for signing.key_file and paths.state_dir when the user omits
// them — supports running without root or hand-creating /var/lib paths.

func xdgEnv(env, fallbackRel string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Last-resort: cwd. validateSigning enforces absolute paths so this
		// fallback is mostly defensive — most environments have $HOME.
		return filepath.Join(".", fallbackRel)
	}
	return filepath.Join(home, fallbackRel)
}

// XDGDataHome returns $XDG_DATA_HOME or ~/.local/share.
func XDGDataHome() string { return xdgEnv("XDG_DATA_HOME", ".local/share") }

// XDGStateHome returns $XDG_STATE_HOME or ~/.local/state.
func XDGStateHome() string { return xdgEnv("XDG_STATE_HOME", ".local/state") }

// DefaultKeyFile is the XDG-derived signing key path used when
// signing.key_file is omitted.
func DefaultKeyFile() string {
	return filepath.Join(XDGDataHome(), "signpost", "secring.gpg")
}

// DefaultStateDir is the XDG-derived state directory used when
// paths.state_dir is omitted.
func DefaultStateDir() string {
	return filepath.Join(XDGStateHome(), "signpost")
}
