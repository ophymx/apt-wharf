package stage

import (
	"bytes"
	"fmt"
	"text/template"
)

// Vars is the closed variable set every .tmpl file is rendered against.
//
// Per cooper-design.md §"Templates": no funcMap, no now(), no env. The
// list is a one-way door — additions become part of build_inputs_hash
// once shipped.
type Vars struct {
	Name        string
	Version     string
	Arch        string
	Epoch       int
	PublishedAt string
}

// PlaceholderVars returns a stand-in Vars for `cooper validate`. The
// values are deliberately realistic-looking so type-mismatch errors in
// templates surface at validate time even when the user hasn't run
// discover yet.
func PlaceholderVars() Vars {
	return Vars{
		Name:        "PLACEHOLDER",
		Version:     "0.0.0",
		Arch:        "amd64",
		Epoch:       0,
		PublishedAt: "2000-01-01T00:00:00Z",
	}
}

// RenderTemplate parses `body` as a Go text/template (with the given
// `name` for error messages), executes it against `vars`, and returns
// the rendered bytes. Strict missing-key handling means a typo like
// `{{ .Versoin }}` errors at execute time.
func RenderTemplate(name string, body []byte, vars Vars) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var out bytes.Buffer
	if err := t.Execute(&out, vars); err != nil {
		return nil, fmt.Errorf("execute %s: %w", name, err)
	}
	return out.Bytes(), nil
}
