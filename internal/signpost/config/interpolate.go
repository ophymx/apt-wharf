package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv expands ${VAR} references in raw config bytes against the process
// environment. A reference whose variable is unset or empty is a hard error.
// Empty fall-through would silently produce a misconfigured daemon — the
// strict-error policy demands a loud failure here.
func ExpandEnv(raw []byte) ([]byte, error) {
	var missing []string
	expanded := envRefPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := envRefPattern.FindSubmatch(match)[1]
		val, ok := os.LookupEnv(string(name))
		if !ok || val == "" {
			missing = append(missing, string(name))
			return match
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset or empty environment variables: %s",
			strings.Join(dedupe(missing), ", "))
	}
	return expanded, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
