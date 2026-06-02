// Package secret loads env-or-file-backed secret values shared by every
// apt-wharf tool — signpost's GitHub token, cooper's GitHub token, etc.
// The "secret" name reflects intent: the values must not be logged or
// committed, and []byte storage exists so callers can best-effort wipe
// after use.
package secret

import (
	"bytes"
	"fmt"
	"os"
)

// Secret is a non-empty value loaded from either an env var or a file.
// Stored as []byte so we can best-effort zero on shutdown — Go strings are
// immutable and cannot be wiped.
type Secret struct {
	Value  []byte
	Origin string // "env:NAME" or "file:/path"
}

// Zero overwrites the stored bytes. Best-effort — runtime may have copied.
func (s *Secret) Zero() {
	for i := range s.Value {
		s.Value[i] = 0
	}
}

// LoadSecret resolves an env-or-file secret pair, enforcing mutual exclusion
// and the "empty resolves to startup failure" rule.
//
// envName/fileName are the user-facing config field names used in error text.
func LoadSecret(envVar, fileVar, envName, fileName string) (*Secret, error) {
	if envVar != "" && fileVar != "" {
		return nil, fmt.Errorf("%s and %s are mutually exclusive", envName, fileName)
	}
	switch {
	case envVar != "":
		raw, ok := os.LookupEnv(envVar)
		if !ok {
			return nil, fmt.Errorf("%s names env var %q which is not set", envName, envVar)
		}
		trimmed := bytes.TrimSpace([]byte(raw))
		if len(trimmed) == 0 {
			return nil, fmt.Errorf("%s names env var %q which is empty", envName, envVar)
		}
		return &Secret{Value: trimmed, Origin: "env:" + envVar}, nil
	case fileVar != "":
		raw, err := os.ReadFile(fileVar)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", fileName, fileVar, err)
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			return nil, fmt.Errorf("%s %s is empty", fileName, fileVar)
		}
		return &Secret{Value: trimmed, Origin: "file:" + fileVar}, nil
	default:
		return nil, nil
	}
}
