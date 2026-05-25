package stage

import (
	"encoding/base64"
	"fmt"
	"os"

	"github.com/ophymx/apt-wharf/pkg/plan"
)

// MaterializeAuxFiles reads the bytes for each Ref, renders templates
// against vars, base64-encodes the result, and returns the BuildPlan
// aux_files map. Map keys are the source-relative path the user wrote
// (or, for glob/dir refs, the "./<rel-to-pkgdir>" form), matching
// cooper-design.md §"Discover JSON contract".
func MaterializeAuxFiles(refs []Ref, vars Vars) (map[string]plan.AuxFile, error) {
	out := make(map[string]plan.AuxFile, len(refs))
	for _, r := range refs {
		body, err := os.ReadFile(r.AbsPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", r.AbsPath, err)
		}
		if r.Template {
			body, err = RenderTemplate(r.Key, body, vars)
			if err != nil {
				return nil, err
			}
		}
		out[r.Key] = plan.AuxFile{ContentB64: base64.StdEncoding.EncodeToString(body)}
	}
	return out, nil
}
