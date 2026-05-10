package plan

import (
	"encoding/json"
	"fmt"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// Canonicalize returns the JCS (RFC 8785) canonical form of v's JSON
// encoding. Used as the input to any hash that's part of the plan
// contract; exposed so external producers can verify their hashes match
// cooper's algorithm.
func Canonicalize(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("jcs transform: %w", err)
	}
	return out, nil
}

// CanonicalizeJSON is Canonicalize for callers that already have JSON
// bytes (e.g. a parsed RawMessage they want to canonicalize).
func CanonicalizeJSON(jsonBytes []byte) ([]byte, error) {
	out, err := jcs.Transform(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("jcs transform: %w", err)
	}
	return out, nil
}
