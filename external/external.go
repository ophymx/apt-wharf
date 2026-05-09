// Package external implements the apt-signpost external-discovery stdio
// contract for tools written in Go. The contract is documented at length in
// apt-signpost's design.md; the wire format is small enough to summarize:
//
//	stdin:  {"prev": {"url": "...", "token": "..."}}   (prev omitted on cold start)
//	stdout: {"url": "...", "token": "..."}             on change
//	stdout: {"unchanged": true}                        on no-op
//	stderr: free-form, captured into signpost logs
//	exit:   0 on success, non-zero on error
//
// Minimal tool:
//
//	func main() {
//	    if err := external.Run(discover); err != nil {
//	        fmt.Fprintln(os.Stderr, "my-discoverer:", err)
//	        os.Exit(1)
//	    }
//	}
//
//	func discover(ctx context.Context, prev *external.Probe) (external.Output, error) {
//	    version, err := fetchUpstreamVersion(ctx)
//	    if err != nil {
//	        return external.Output{}, err
//	    }
//	    if prev != nil && prev.Token == version {
//	        return external.Output{Unchanged: true}, nil
//	    }
//	    return external.Output{
//	        URL:   "https://example.com/foo-" + version + ".deb",
//	        Token: version,
//	    }, nil
//	}
//
// Tools using this package can be exercised outside the signpost runtime by
// piping JSON in and out of the binary directly:
//
//	$ echo '{}' | ./my-discoverer
//	{"url":"...","token":"..."}
//	$ echo '{"prev":{"url":"...","token":"v1"}}' | ./my-discoverer
//	{"unchanged":true}
package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Input is the JSON envelope apt-signpost sends on stdin. Prev is nil on
// cold start (the JSON object is "{}" rather than `{"prev":null}`).
type Input struct {
	Prev *Probe `json:"prev,omitempty"`
}

// Probe carries the URL and opaque token apt-signpost remembers across
// refresh ticks. Token is whatever string the discoverer chose last time —
// often a version, a release id, or an HTTP validator. The discoverer's
// only job for change detection is "did this token change?"
type Probe struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Output is what the tool writes to stdout. Either URL+Token are set, or
// Unchanged is true; mixing the two is rejected by Validate.
type Output struct {
	URL       string `json:"url,omitempty"`
	Token     string `json:"token,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

// Validate enforces the response invariant: exactly one of (URL+Token) or
// (Unchanged=true). WriteOutput calls this before emitting, so most tools
// don't need to invoke it directly.
func (o Output) Validate() error {
	if o.Unchanged {
		if o.URL != "" || o.Token != "" {
			return errors.New("Output: unchanged cannot be combined with url/token")
		}
		return nil
	}
	if o.URL == "" || o.Token == "" {
		return errors.New("Output: url and token are both required when unchanged is false")
	}
	return nil
}

// ProbeFunc is the user-supplied discovery callback. prev is nil on cold
// start. The returned Output is validated before being written to stdout.
type ProbeFunc func(ctx context.Context, prev *Probe) (Output, error)

// Run reads Input from os.Stdin, calls fn, validates its Output, and writes
// it to os.Stdout. The context handed to fn is cancelled on SIGINT/SIGTERM
// so HTTP calls inside fn can shut down promptly when apt-signpost decides
// to terminate the discoverer.
//
// Run returns an error rather than calling os.Exit so the caller controls
// exit semantics; the typical pattern is:
//
//	if err := external.Run(discover); err != nil {
//	    fmt.Fprintln(os.Stderr, "my-discoverer:", err)
//	    os.Exit(1)
//	}
func Run(fn ProbeFunc) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return RunWith(ctx, os.Stdin, os.Stdout, fn)
}

// RunWith is the testable form of Run. I/O is injected and signal handling
// is left to the caller. Use Run from main(); use RunWith from tests.
func RunWith(ctx context.Context, stdin io.Reader, stdout io.Writer, fn ProbeFunc) error {
	in, err := ReadInput(stdin)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	out, err := fn(ctx, in.Prev)
	if err != nil {
		return err
	}
	return WriteOutput(stdout, out)
}

// ReadInput parses an Input from r. Empty (or whitespace-only) input is
// tolerated and treated as a cold-start ({}) — apt-signpost always sends a
// JSON object, but tools may also be exercised by hand from a shell with
// `: | ./my-discoverer` or similar.
func ReadInput(r io.Reader) (*Input, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	in := &Input{}
	if len(strings.TrimSpace(string(body))) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(body, in); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	return in, nil
}

// WriteOutput validates out and serializes it to w as one JSON object
// followed by a newline.
func WriteOutput(w io.Writer, out Output) error {
	if err := out.Validate(); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(out)
}
