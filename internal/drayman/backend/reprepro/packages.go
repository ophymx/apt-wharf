package reprepro

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
)

// parsePackages reads a Debian Packages file (a sequence of RFC822
// stanzas separated by blank lines) and returns the subset of fields
// drayman cares about. Continuation lines starting with whitespace
// extend the previous field; drayman only reads single-line fields
// (Package, Version, Architecture, X-Cooper-Build-Inputs-Hash), so a
// minimal parser suffices.
//
// Returns an empty slice for empty input. Malformed stanzas are
// skipped (no record emitted) rather than failing the whole parse —
// drayman's purpose is to query for known fields, so a broken stanza
// is just "not found".
func parsePackages(body []byte) []backend.Package {
	var out []backend.Package
	cur := backend.Package{}
	hasFields := false

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if hasFields && cur.Name != "" {
				out = append(out, cur)
			}
			cur = backend.Package{}
			hasFields = false
			continue
		}
		// Continuation lines (start with whitespace) extend the
		// previous field. drayman doesn't need them.
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		hasFields = true
		switch key {
		case "Package":
			cur.Name = value
		case "Version":
			cur.Version = value
		case "Architecture":
			cur.Architecture = value
		case "X-Cooper-Build-Inputs-Hash":
			cur.BuildInputsHash = value
		}
	}
	// Trailing stanza without a closing blank line.
	if hasFields && cur.Name != "" {
		out = append(out, cur)
	}
	return out
}
