package stage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// CloneNfpm deep-copies a yaml.Node so per-arch substitution doesn't
// mutate a shared parse tree. yaml.v3 has no Clone helper; round-tripping
// through Marshal/Unmarshal is the cleanest approach and produces a
// fully independent tree.
func CloneNfpm(n *yaml.Node) (*yaml.Node, error) {
	if n == nil {
		return nil, nil
	}
	buf, err := yaml.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("clone marshal: %w", err)
	}
	var out yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(buf)).Decode(&out); err != nil {
		return nil, fmt.Errorf("clone decode: %w", err)
	}
	return &out, nil
}

// SubstituteNfpm walks every string scalar in n and replaces literal
// ${VERSION} and ${ARCH} occurrences with version and arch. Other
// substitution surfaces (notably ${ASSETS}, plus any nfpm-time variables
// the user added) pass through verbatim — nfpm itself expands those at
// build time per cooper-design.md §"Cooper's substitution into doc 2".
func SubstituteNfpm(n *yaml.Node, version, arch string) {
	walkScalars(n, func(s string) string {
		s = strings.ReplaceAll(s, "${VERSION}", version)
		s = strings.ReplaceAll(s, "${ARCH}", arch)
		return s
	})
}

// walkScalars applies fn to the .Value of every string-typed scalar node
// reachable from n. Non-string scalars (integers, booleans, nulls) are
// untouched so type information survives the walk.
func walkScalars(n *yaml.Node, fn func(string) string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && (n.Tag == "" || n.Tag == "!!str") {
		n.Value = fn(n.Value)
	}
	for _, c := range n.Content {
		walkScalars(c, fn)
	}
}

// NfpmToJSON converts the nfpm doc 2 yaml.Node into the json.RawMessage
// shape that BuildPlan.Nfpm requires. yaml.v3 decodes into map[string]any
// for ordinary mappings; integer modes (0o755 → 493 etc.) survive
// decoding as int and re-marshal as JSON numbers per cooper-design.md
// §"Discover JSON contract" ("File-info modes are decimal in JSON").
func NfpmToJSON(n *yaml.Node) (json.RawMessage, error) {
	if n == nil {
		return nil, fmt.Errorf("nil nfpm node")
	}
	root, err := documentRoot(n)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := root.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode nfpm: %w", err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal nfpm: %w", err)
	}
	return out, nil
}
