package stage

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// documentRoot unwraps a yaml.DocumentNode to its single mapping child.
// Accepts an already-unwrapped node as well.
func documentRoot(n *yaml.Node) (*yaml.Node, error) {
	if n == nil {
		return nil, fmt.Errorf("nil yaml node")
	}
	root := n
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return nil, fmt.Errorf("expected one root node, got %d", len(root.Content))
		}
		root = root.Content[0]
	}
	return root, nil
}

// childNode returns the value node for `key` under a MappingNode, or nil
// if absent or root isn't a mapping.
func childNode(root *yaml.Node, key string) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

// scriptsMap returns the scripts: subnode if present and a mapping.
func scriptsMap(root *yaml.Node) *yaml.Node {
	n := childNode(root, "scripts")
	if n != nil && n.Kind == yaml.MappingNode {
		return n
	}
	return nil
}

// lookupString reads a scalar string value at `key`. Returns ok=false if
// the key is absent or its value isn't a scalar string.
func lookupString(m *yaml.Node, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v := childNode(m, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return "", false
	}
	var s string
	if err := v.Decode(&s); err != nil {
		return "", false
	}
	return s, true
}

// collectContentsSrc returns the src: scalars from contents[]. Entries
// that don't have a scalar src: are skipped — nfpm itself will reject
// malformed entries at build time. Entries with type: symlink are
// skipped too: nfpm interprets symlink src: as the link's *target
// string*, not a file path on disk, so the aux-file collector must
// not try to resolve it.
func collectContentsSrc(root *yaml.Node) []string {
	contents := childNode(root, "contents")
	if contents == nil || contents.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, item := range contents.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if t, ok := lookupString(item, "type"); ok && t == "symlink" {
			continue
		}
		s, ok := lookupString(item, "src")
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}
