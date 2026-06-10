package stage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
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

// ArchGNUTable maps Debian arch keys to their GNU/uname spelling, used
// to resolve ${ARCH_GNU} in doc 2 per cooper-design.md §"Built-in arch
// aliases". The table is the contract surface for new arches: adding
// rows is non-breaking; renaming or removing a row bumps
// pkg/plan.FormatRevision because resolved build_plan.nfpm bytes would
// diverge.
var ArchGNUTable = map[string]string{
	"amd64":   "x86_64",
	"arm64":   "aarch64",
	"armhf":   "armv7l",
	"riscv64": "riscv64",
}

// passthroughVars are the substitution names cooper leaves symbolic for
// nfpm's env mapper to expand at build time. Other unresolved ${KEY}
// references in doc 2 are discover errors per the strict substitution
// rule.
var passthroughVars = map[string]bool{
	"ASSETS": true,
	"SOURCE": true,
}

// substRefPattern matches every ${KEY} occurrence whose key conforms to
// the cooper substitution name grammar — same shape as
// archVarKeyPattern in internal/cooper/config. Strings with non-conforming
// embedded ${...} sequences (lowercase, punctuation) pass through
// untouched so legitimate text like ${pwd} in a description isn't
// silently rewritten or flagged.
var substRefPattern = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

// BuildSubs returns the discover-time substitution map for one arch:
// the built-in names (VERSION, ARCH, ARCH_GNU when arch is in
// ArchGNUTable) plus per-arch user vars. Built-ins are written after
// archVars so a malformed sidecar that slipped past validation can't
// silently shadow them; config.validateArchVars is the enforced gate.
func BuildSubs(version, arch string, archVars map[string]string) map[string]string {
	subs := make(map[string]string, len(archVars)+3)
	maps.Copy(subs, archVars)
	subs["VERSION"] = version
	subs["ARCH"] = arch
	if v, ok := ArchGNUTable[arch]; ok {
		subs["ARCH_GNU"] = v
	}
	return subs
}

// SubstituteNfpm walks every string scalar in n and substitutes
// ${KEY} references from subs. Names in passthroughVars (ASSETS,
// SOURCE) are left verbatim for nfpm to expand at build time.
//
// Unresolved references produce typed errors so callers can map them
// to the right plan.Error.Kind: a reference to ${ARCH_GNU} for an arch
// not in ArchGNUTable becomes an *ArchGNUUnknownError whose message
// prints the supported table; any other unresolved ${KEY} becomes an
// *UnresolvedSubstitutionError with one entry per (key, doc-2 path)
// occurrence. Both errors surface every offending reference in a
// single pass — recipes get the full list, not one-at-a-time
// whack-a-mole.
//
// arch is carried for diagnostics only; subs already encodes its
// effects. See cooper-design.md §"Cooper's substitution into doc 2"
// for the contract.
func SubstituteNfpm(n *yaml.Node, arch string, subs map[string]string) error {
	var (
		unresolved  []unresolvedRef
		archGNURefs []string
		seenArchGNU = make(map[string]bool)
		seenOther   = make(map[string]bool)
	)
	walkScalarsWithPath(n, "", func(path, value string) string {
		return substRefPattern.ReplaceAllStringFunc(value, func(match string) string {
			key := match[2 : len(match)-1]
			if v, ok := subs[key]; ok {
				return v
			}
			if passthroughVars[key] {
				return match
			}
			if key == "ARCH_GNU" {
				if !seenArchGNU[path] {
					seenArchGNU[path] = true
					archGNURefs = append(archGNURefs, path)
				}
				return match
			}
			tag := key + "@" + path
			if !seenOther[tag] {
				seenOther[tag] = true
				unresolved = append(unresolved, unresolvedRef{Key: key, Path: path})
			}
			return match
		})
	})
	if len(archGNURefs) > 0 {
		return &ArchGNUUnknownError{Arch: arch, Refs: archGNURefs}
	}
	if len(unresolved) > 0 {
		return &UnresolvedSubstitutionError{Arch: arch, Refs: unresolved}
	}
	return nil
}

// unresolvedRef is one (key, doc-2 path) reference that cooper could
// not satisfy at discover time.
type unresolvedRef struct {
	Key  string
	Path string
}

// UnresolvedSubstitutionError reports ${KEY} references in doc 2 that
// don't resolve against the built-in set or the recipe's per-arch
// vars. Maps to plan.ErrorKindUnresolvedSubstitution.
type UnresolvedSubstitutionError struct {
	Arch string
	Refs []unresolvedRef
}

func (e *UnresolvedSubstitutionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "doc 2 references unresolved ${KEY} substitutions for arch %q:\n", e.Arch)
	for _, r := range e.Refs {
		fmt.Fprintf(&b, "  - ${%s} at %s\n", r.Key, displayPath(r.Path))
	}
	b.WriteString("Fix: declare each key in arches[")
	b.WriteString(e.Arch)
	b.WriteString("].vars (uppercase, [A-Z][A-Z0-9_]*; cannot shadow VERSION/ARCH/ARCH_GNU/ASSETS/SOURCE), or remove the reference from doc 2.")
	return b.String()
}

// ArchGNUUnknownError reports ${ARCH_GNU} references in doc 2 against
// an arch cooper has no GNU spelling for. Distinct from the generic
// unresolved-substitution case so orchestrators can distinguish "the
// recipe asked for a built-in that isn't covered" from "the recipe
// referenced an undeclared user var." Maps to
// plan.ErrorKindArchGNUUnknown.
type ArchGNUUnknownError struct {
	Arch string
	Refs []string // doc-2 paths where ${ARCH_GNU} appeared
}

func (e *ArchGNUUnknownError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "doc 2 references ${ARCH_GNU} for arch %q but cooper has no GNU mapping for it.\n", e.Arch)
	b.WriteString("References:\n")
	for _, p := range e.Refs {
		fmt.Fprintf(&b, "  - %s\n", displayPath(p))
	}
	b.WriteString("\nSupported ARCH → ARCH_GNU:\n")
	for _, k := range sortedKeys(ArchGNUTable) {
		fmt.Fprintf(&b, "  %-8s → %s\n", k, ArchGNUTable[k])
	}
	b.WriteString("\nFix: drop the ${ARCH_GNU} reference from doc 2, or pick a different per-arch arches[")
	b.WriteString(e.Arch)
	b.WriteString("].vars key (e.g. TARBALL_ARCH) for the literal the recipe needs. vars cannot shadow ARCH_GNU itself — adding a row to cooper's built-in table is the supported path for new arches.")
	return b.String()
}

// sortedKeys is the small map-key sort used by ArchGNUUnknownError's
// message. Standalone because it's also useful in future error helpers.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// displayPath formats a YAML path for error messages. Top-level scalars
// (path="") render as "(root)" so the user sees something concrete
// instead of an empty location.
func displayPath(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

// walkScalarsWithPath is walkScalars with a YAML path string threaded
// through the recursion. Mapping keys append `.key` (or appear bare at
// the root); sequence indices append `[i]`. The path matches the
// dotted-field style users see elsewhere (e.g. contents[2].src).
//
// Aliases are NOT recursed into — yaml.v3 resolves anchors at decode
// time for tree-shape nodes but leaves alias nodes pointing at the
// anchor target; walking into them would double-substitute.
func walkScalarsWithPath(n *yaml.Node, path string, fn func(path, value string) string) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkScalarsWithPath(c, path, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			valNode := n.Content[i+1]
			child := joinPath(path, keyNode.Value)
			walkScalarsWithPath(valNode, child, fn)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walkScalarsWithPath(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	case yaml.ScalarNode:
		if n.Tag == "" || n.Tag == "!!str" {
			n.Value = fn(path, n.Value)
		}
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
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
