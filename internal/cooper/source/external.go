package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ophymx/apt-wharf/internal/cooper/config"
	signsource "github.com/ophymx/apt-wharf/internal/source"
)

// defaultExternalTimeout applies when source.external.timeout is omitted.
// Matches signpost's external-discoverer default so the contract feels
// uniform across the two tools.
const defaultExternalTimeout = 30 * time.Second

// externalEnvAllowlist names process env vars forwarded to the
// discovery script. Everything else is dropped so the child sandbox
// stays predictable; the per-source Env map is layered on top of this.
var externalEnvAllowlist = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// ExternalAsset is one element of the script's `assets[]` reply.
// SHA256 is optional: when present cooper trusts it and writes it into
// plan.Asset.SHA256 verbatim; when absent build will stream + hash.
type ExternalAsset struct {
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// ExternalReply is the JSON document the script emits to stdout. The
// shape mirrors cooper-design.md §"External source script contract".
type ExternalReply struct {
	Version string          `json:"version"`
	Assets  []ExternalAsset `json:"assets"`
}

// ExternalResolution is what ResolveExternal hands back to the discover
// orchestrator. Assets keys are arch names (extracted from the script
// reply); cooper rejects an asset whose arch isn't declared in the
// recipe's arches map.
type ExternalResolution struct {
	Version     string
	Command     string // the script path, used as plan.Source.URL
	Assets      map[string]ExternalAsset
	SourceEpoch int64
}

// ResolveExternal exec's the configured script from workdir (the
// cooper.yaml directory), reads {version, assets[]} JSON from stdout,
// and returns the parsed reply plus a deterministically-derived
// source_date_epoch. workdir is also passed to the child as cmd.Dir so
// `./discover.sh`-style relative paths resolve naturally.
//
// The child runs with a stripped environment (proxy allowlist + the
// per-source Env map) and is hard-killed when Timeout elapses. stderr
// is captured and surfaced in error messages so a failing script
// leaves something a recipe author can act on.
func ResolveExternal(ctx context.Context, e *config.ExternalSource, workdir string) (*ExternalResolution, error) {
	if e == nil {
		return nil, errors.New("external: source is nil")
	}
	if len(e.Command) == 0 {
		return nil, errors.New("external: command is empty (validate should have caught this)")
	}

	timeout := defaultExternalTimeout
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil {
			return nil, fmt.Errorf("external: timeout %q: %w", e.Timeout, err)
		}
		timeout = d
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, e.Command[0], e.Command[1:]...)
	cmd.Dir = workdir
	cmd.Env = buildExternalEnv(e.Env)
	// Share signpost's procgroup-SIGKILL + WaitDelay discipline so
	// grandchildren spawned by a wrapper shell can't survive timeout.
	signsource.SetProcAttrs(cmd)
	cmd.WaitDelay = 2 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, runErr := cmd.Output()
	if runErr != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("external %s: timeout after %s%s",
				e.Command[0], timeout, stderrTail(stderrBuf.Bytes()))
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return nil, fmt.Errorf("external %s: exit %d%s",
				e.Command[0], ee.ExitCode(), stderrTail(stderrBuf.Bytes()))
		}
		return nil, fmt.Errorf("external %s: %w", e.Command[0], runErr)
	}

	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("external %s: empty stdout", e.Command[0])
	}
	var reply ExternalReply
	if err := json.Unmarshal(trimmed, &reply); err != nil {
		return nil, fmt.Errorf("external %s: parse stdout: %w", e.Command[0], err)
	}

	if reply.Version == "" {
		return nil, fmt.Errorf("external %s: reply missing required .version", e.Command[0])
	}
	if len(reply.Assets) == 0 {
		return nil, fmt.Errorf("external %s: reply has no assets", e.Command[0])
	}

	assets := make(map[string]ExternalAsset, len(reply.Assets))
	for i, a := range reply.Assets {
		if a.Arch == "" {
			return nil, fmt.Errorf("external %s: assets[%d].arch is empty", e.Command[0], i)
		}
		if a.URL == "" {
			return nil, fmt.Errorf("external %s: assets[%d].url is empty", e.Command[0], i)
		}
		if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("external %s: assets[%d].url %q must use http or https scheme",
				e.Command[0], i, a.URL)
		}
		if a.SHA256 != "" {
			if err := validateSHA256Hex(a.SHA256); err != nil {
				return nil, fmt.Errorf("external %s: assets[%d].sha256: %w", e.Command[0], i, err)
			}
		}
		if prev, ok := assets[a.Arch]; ok {
			return nil, fmt.Errorf("external %s: assets has duplicate arch %q (urls %s and %s)",
				e.Command[0], a.Arch, prev.URL, a.URL)
		}
		assets[a.Arch] = a
	}

	return &ExternalResolution{
		Version:     reply.Version,
		Command:     e.Command[0],
		Assets:      assets,
		SourceEpoch: deriveURLEpoch(e.Command[0], reply.Version),
	}, nil
}

func buildExternalEnv(extra map[string]string) []string {
	out := make([]string, 0, len(externalEnvAllowlist)+len(extra))
	for _, name := range externalEnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// validateSHA256Hex accepts either "sha256:<hex>" or bare hex; in both
// cases the hex portion must be exactly 64 lowercase hex chars. Strict
// because a script-provided hash is used verbatim as plan.Asset.SHA256
// (which downstream consumers parse as "sha256:<hex>").
func validateSHA256Hex(s string) error {
	hex, _ := strings.CutPrefix(s, "sha256:")
	if len(hex) != 64 {
		return fmt.Errorf("expected 64 hex chars, got %d", len(hex))
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return fmt.Errorf("non-hex char %q at offset %d", c, i)
		}
	}
	return nil
}

const externalStderrSnippetMax = 1024

// stderrTail formats the trailing portion of captured stderr for
// inclusion in an error message, matching the signpost helper's shape.
func stderrTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > externalStderrSnippetMax {
		s = s[len(s)-externalStderrSnippetMax:]
	}
	return ": " + s
}
