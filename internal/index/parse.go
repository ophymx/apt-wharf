package index

import (
	"bytes"
	"fmt"

	"pault.ag/go/debian/control"
)

// FieldSet is the trio of control fields the strict checks need.
type FieldSet struct {
	Package, Version, Architecture string
}

// ExtractFields parses a single control paragraph via pault.ag/go/debian's
// RFC822-style parser and returns the relevant fields. Defers all the
// continuation-line / colon-handling rules to that library so we don't have
// to maintain a parallel parser.
func ExtractFields(stanza []byte) (FieldSet, error) {
	pr, err := control.NewParagraphReader(bytes.NewReader(stanza), nil)
	if err != nil {
		return FieldSet{}, fmt.Errorf("paragraph reader: %w", err)
	}
	para, err := pr.Next()
	if err != nil {
		return FieldSet{}, fmt.Errorf("parse control: %w", err)
	}
	if para == nil {
		return FieldSet{}, fmt.Errorf("control stanza is empty")
	}

	fs := FieldSet{
		Package:      para.Values["Package"],
		Version:      para.Values["Version"],
		Architecture: para.Values["Architecture"],
	}
	if fs.Package == "" {
		return fs, fmt.Errorf("control stanza missing Package field")
	}
	if fs.Version == "" {
		return fs, fmt.Errorf("control stanza missing Version field")
	}
	if fs.Architecture == "" {
		return fs, fmt.Errorf("control stanza missing Architecture field")
	}
	return fs, nil
}
