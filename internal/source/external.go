package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// envAllowlist names process env vars that are forwarded to external
// commands. Per design.md these are the only inherited variables; everything
// else is dropped to keep the child sandbox predictable. Per-source env from
// the YAML is layered on top.
var envAllowlist = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// defaultExternalTimeout applies when discovery.timeout is omitted.
const defaultExternalTimeout = 30 * time.Second

// ExternalDiscoverer runs an admin-supplied command per refresh tick, piping
// the stdio JSON contract documented in design.md:
//
//	stdin:  {"prev": {"url": "...", "token": "..."}}   (prev omitted on cold start)
//	stdout: {"url": "...", "token": "..."}             on change
//	stdout: {"unchanged": true}                        on no-op
//	stderr: free-form, captured into signpost logs
//	exit:   0 = success, anything else = error
//
// The child runs with a stripped environment (envAllowlist + the per-source
// Env map) and is hard-killed when Timeout elapses.
type ExternalDiscoverer struct {
	Command []string
	Timeout time.Duration
	Env     map[string]string

	// Name and Logger drive the stderr-capture log lines. Logger may be nil
	// to silence them; Name is the source key from config and shows up in
	// every log record so multi-source deployments stay legible.
	Name   string
	Logger *slog.Logger
}

func NewExternalDiscoverer(command []string, timeout time.Duration, env map[string]string, name string, logger *slog.Logger) (*ExternalDiscoverer, error) {
	if len(command) == 0 {
		return nil, errors.New("external: command is required")
	}
	if timeout <= 0 {
		timeout = defaultExternalTimeout
	}
	return &ExternalDiscoverer{
		Command: command,
		Timeout: timeout,
		Env:     env,
		Name:    name,
		Logger:  logger,
	}, nil
}

type externalInput struct {
	Prev *externalProbe `json:"prev,omitempty"`
}

type externalProbe struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type externalOutput struct {
	URL       string `json:"url,omitempty"`
	Token     string `json:"token,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

func (d *ExternalDiscoverer) Probe(ctx context.Context, in ProbeInput) (*ProbeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, d.Command[0], d.Command[1:]...)
	cmd.Env = d.buildEnv()
	// On unix, place the child in its own process group and SIGKILL the
	// whole group on context cancel — otherwise grandchildren (e.g. `sleep`
	// invoked by a wrapper shell) would survive and keep stdio pipes open.
	// WaitDelay is a belt-and-suspenders cap in case the kill races.
	setProcAttrs(cmd)
	cmd.WaitDelay = 2 * time.Second

	payload := externalInput{}
	if in.Prev != nil {
		payload.Prev = &externalProbe{URL: in.Prev.URL, Token: in.Prev.Token}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("external %s: marshal stdin: %w", d.Command[0], err)
	}
	cmd.Stdin = bytes.NewReader(body)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdoutBuf, runErr := cmd.Output()
	d.logStderr(stderrBuf.Bytes(), runErr != nil)

	if runErr != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("external %s: timeout after %s%s",
				d.Command[0], d.Timeout, stderrTail(stderrBuf.Bytes()))
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return nil, fmt.Errorf("external %s: exit %d%s",
				d.Command[0], ee.ExitCode(), stderrTail(stderrBuf.Bytes()))
		}
		return nil, fmt.Errorf("external %s: %w", d.Command[0], runErr)
	}

	out := externalOutput{}
	trimmed := bytes.TrimSpace(stdoutBuf)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("external %s: empty stdout", d.Command[0])
	}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, fmt.Errorf("external %s: parse stdout: %w (raw=%q)",
			d.Command[0], err, snippet(trimmed))
	}

	if out.Unchanged {
		if in.Prev == nil {
			return nil, fmt.Errorf("external %s: returned unchanged but no prev was sent",
				d.Command[0])
		}
		return &ProbeResult{Probe: *in.Prev, Unchanged: true}, nil
	}

	if out.URL == "" || out.Token == "" {
		return nil, fmt.Errorf("external %s: response must include both url and token", d.Command[0])
	}

	res := &ProbeResult{Probe: Probe{URL: out.URL, Token: out.Token}}
	if in.Prev != nil && in.Prev.Token == out.Token {
		res.Unchanged = true
	}
	return res, nil
}

func (d *ExternalDiscoverer) buildEnv() []string {
	env := make([]string, 0, len(envAllowlist)+len(d.Env))
	for _, name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for k, v := range d.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (d *ExternalDiscoverer) logStderr(b []byte, isError bool) {
	if d.Logger == nil || len(bytes.TrimSpace(b)) == 0 {
		return
	}
	level := slog.LevelDebug
	if isError {
		level = slog.LevelInfo
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		d.Logger.Log(context.Background(), level, "external stderr",
			"source", d.Name, "line", line)
	}
}

const stderrSnippetMax = 1024

// stderrTail returns up to the last 1 KiB of stderr formatted for inclusion
// in an error message (": <text>"), or the empty string if stderr is empty.
func stderrTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > stderrSnippetMax {
		s = s[len(s)-stderrSnippetMax:]
	}
	return ": " + s
}

func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
