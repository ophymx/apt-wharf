package discover

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// vars is the closed variable set exposed to chandler templates. Adding
// a field here is the only way to grow the template surface — every
// addition becomes part of build_inputs_hash once shipped.
type vars struct {
	Distro   string
	Codename string
}

// expand renders s against v with Go text/template. Strings without
// template markers are returned as-is — the fast path avoids spinning
// up a parser for the vast majority of fields that don't template.
func expand(s string, v vars) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	tmpl, err := template.New("chandler").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", s, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("render template %q: %w", s, err)
	}
	return buf.String(), nil
}

// expandList applies expand to every element, preserving order. Returns
// nil for nil input (vs. an empty []string), so a missing list stays
// missing in the resolved struct.
func expandList(xs []string, v vars) ([]string, error) {
	if xs == nil {
		return nil, nil
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		r, err := expand(x, v)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
