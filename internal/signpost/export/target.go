package export

import (
	"fmt"
	"sort"
	"strings"
)

// Target renders host-specific redirect and header rules for an export.
// Targets return files rather than writing them so Write can apply one
// atomic-write + prune discipline to everything in the output root.
type Target interface {
	Name() string
	// Emit returns extra files keyed by output-root-relative path. The
	// manifest is the validated, sorted view of the snapshot and is the
	// only input a target gets.
	Emit(m *Manifest) (map[string][]byte, error)
}

var targets = map[string]Target{
	"cloudflare": cloudflareTarget{},
	"nginx":      nginxTarget{},
	"manifest":   manifestTarget{},
}

// TargetNames lists the supported target names, sorted.
func TargetNames() []string {
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseTargets resolves a list of names (each possibly comma-separated)
// into Targets, rejecting unknown names and duplicates.
func ParseTargets(specs []string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for _, spec := range specs {
		for name := range strings.SplitSeq(spec, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			t, ok := targets[name]
			if !ok {
				return nil, fmt.Errorf("unknown target %q (supported: %s)", name, strings.Join(TargetNames(), ", "))
			}
			if seen[name] {
				return nil, fmt.Errorf("target %q given more than once", name)
			}
			seen[name] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no target given (supported: %s)", strings.Join(TargetNames(), ", "))
	}
	return out, nil
}
