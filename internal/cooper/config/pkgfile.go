package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PackageFile is a parsed per-package multi-doc file. Sidecar is the
// strictly-validated doc 1; Nfpm is doc 2 held verbatim as a yaml.Node so
// downstream phases can re-emit it after substituting cooper's three
// variables. NfpmName is doc 2's top-level name: extracted up front
// because cooper uses it to name .deb files and as the staging-dir
// component.
type PackageFile struct {
	Path     string
	Dir      string
	Sidecar  Sidecar
	Nfpm     yaml.Node
	NfpmName string
}

// LoadPackage reads, splits, decodes, and validates one per-package
// multi-doc YAML file. Validation is strict for doc 1 (KnownFields
// equivalent) and minimal for doc 2 (must be a mapping with a non-empty
// name: string) — anything else in doc 2 is passed through to nfpm at
// build time.
func LoadPackage(path string) (*PackageFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", abs, err)
	}

	docs, err := splitDocs(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	if len(docs) != 2 {
		return nil, fmt.Errorf("%s: expected exactly 2 YAML documents, got %d", abs, len(docs))
	}

	sidecar, err := decodeSidecarStrict(docs[0])
	if err != nil {
		return nil, fmt.Errorf("%s: doc 1 (cooper sidecar): %w", abs, err)
	}
	if err := validateSidecar(&sidecar); err != nil {
		return nil, fmt.Errorf("%s: doc 1 (cooper sidecar): %w", abs, err)
	}

	nfpmNode := docs[1]
	name, err := extractNfpmName(nfpmNode)
	if err != nil {
		return nil, fmt.Errorf("%s: doc 2 (nfpm): %w", abs, err)
	}

	return &PackageFile{
		Path:     abs,
		Dir:      filepath.Dir(abs),
		Sidecar:  sidecar,
		Nfpm:     *nfpmNode,
		NfpmName: name,
	}, nil
}

// splitDocs decodes every YAML document in raw into a slice of yaml.Nodes.
// Returns an error if the file contains zero parseable documents.
func splitDocs(raw []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("YAML parse: %w", err)
		}
		out = append(out, &n)
	}
	return out, nil
}

// decodeSidecarStrict re-marshals a yaml.Node and runs a strict (KnownFields)
// decode against the typed Sidecar. The detour is the only way to get
// yaml.v3 strictness out of an already-parsed Node.
func decodeSidecarStrict(n *yaml.Node) (Sidecar, error) {
	buf, err := yaml.Marshal(n)
	if err != nil {
		return Sidecar{}, fmt.Errorf("re-marshal: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf))
	dec.KnownFields(true)
	var s Sidecar
	if err := dec.Decode(&s); err != nil {
		return Sidecar{}, err
	}
	return s, nil
}

// extractNfpmName pulls the top-level name: string out of doc 2. Doc 2
// must be a mapping; anything else is rejected so nfpm doesn't get a
// surprise input.
func extractNfpmName(n *yaml.Node) (string, error) {
	root := n
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return "", fmt.Errorf("expected exactly one root node, got %d", len(root.Content))
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return "", fmt.Errorf("must be a YAML mapping at top level")
	}
	for i := 0; i < len(root.Content); i += 2 {
		k := root.Content[i]
		v := root.Content[i+1]
		if k.Value != "name" {
			continue
		}
		var s string
		if err := v.Decode(&s); err != nil {
			return "", fmt.Errorf("name: %w", err)
		}
		if s == "" {
			return "", fmt.Errorf("name: must be a non-empty string")
		}
		return s, nil
	}
	return "", fmt.Errorf("missing required top-level name:")
}
