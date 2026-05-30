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
// strictly-validated doc 1; NfpmDocs holds one entry per produced .deb
// (docs 2..N), each preserved verbatim as a yaml.Node so downstream
// phases can re-emit them after substituting cooper's three
// variables.
//
// The N>=1 shape lets a single source resolution fan out into
// multiple .debs that share the same upstream provenance (one
// release, one URL fetch, one set of staged assets) but split into
// distinct apt packages — e.g. `ollama` + `libollama-nvidia` from
// one ollama-linux-<arch>.tgz release. v0 recipes (exactly two YAML
// documents → exactly one nfpm doc) are the singleton case.
type PackageFile struct {
	Path     string
	Dir      string
	Sidecar  Sidecar
	NfpmDocs []NfpmDoc
}

// NfpmDoc is one nfpm document from the recipe — corresponds to one
// produced .deb. Name is the doc's top-level `name:` extracted up
// front because cooper uses it to name .deb files and as the
// staging-dir component.
type NfpmDoc struct {
	Node yaml.Node
	Name string
}

// LoadPackage reads, splits, decodes, and validates one per-package
// multi-doc YAML file. Validation is strict for doc 1 (KnownFields
// equivalent) and minimal for docs 2..N (each must be a mapping
// with a non-empty `name:` string, and names must be unique within
// the file) — anything else passes through to nfpm at build time.
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
	if len(docs) < 2 {
		return nil, fmt.Errorf("%s: expected at least 2 YAML documents (1 sidecar + N nfpm), got %d", abs, len(docs))
	}

	sidecar, err := decodeSidecarStrict(docs[0])
	if err != nil {
		return nil, fmt.Errorf("%s: doc 1 (cooper sidecar): %w", abs, err)
	}
	if err := validateSidecar(&sidecar, filepath.Dir(abs)); err != nil {
		return nil, fmt.Errorf("%s: doc 1 (cooper sidecar): %w", abs, err)
	}

	nfpmDocs := make([]NfpmDoc, 0, len(docs)-1)
	seenNames := make(map[string]int, len(docs)-1)
	for i := 1; i < len(docs); i++ {
		docLabel := fmt.Sprintf("doc %d (nfpm)", i+1)
		name, err := extractNfpmName(docs[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", abs, docLabel, err)
		}
		if prev, dup := seenNames[name]; dup {
			return nil, fmt.Errorf("%s: %s: package name %q duplicates doc %d", abs, docLabel, name, prev+1)
		}
		seenNames[name] = i
		nfpmDocs = append(nfpmDocs, NfpmDoc{Node: *docs[i], Name: name})
	}

	return &PackageFile{
		Path:     abs,
		Dir:      filepath.Dir(abs),
		Sidecar:  sidecar,
		NfpmDocs: nfpmDocs,
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
