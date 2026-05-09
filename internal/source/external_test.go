package source

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// makeScript writes a tiny POSIX shell script in t.TempDir and returns its
// absolute path. Body is appended after the shebang. Skips on non-POSIX hosts.
func makeScript(t *testing.T, body string) string {
	t.Helper()
	return makeScriptIn(t, t.TempDir(), body)
}

func makeScriptIn(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("external discoverer tests use POSIX shell scripts")
	}
	path := filepath.Join(dir, "discover.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestExternal_ColdStartEmitsURLAndToken(t *testing.T) {
	script := makeScript(t, `printf '%s\n' '{"url":"https://example.com/foo.deb","token":"v1"}'`)
	d, err := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.URL != "https://example.com/foo.deb" || res.Probe.Token != "v1" {
		t.Errorf("got %+v, want url=foo.deb token=v1", res.Probe)
	}
	if res.Unchanged {
		t.Errorf("Unchanged=true on cold start")
	}
}

func TestExternal_UnchangedResponse(t *testing.T) {
	script := makeScript(t, `printf '%s\n' '{"unchanged": true}'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	prev := &Probe{URL: "https://x", Token: "v9"}
	res, err := d.Probe(context.Background(), ProbeInput{Prev: prev})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Unchanged {
		t.Errorf("Unchanged=false, want true")
	}
	if res.Probe.URL != prev.URL || res.Probe.Token != prev.Token {
		t.Errorf("Probe should mirror prev when unchanged: got %+v", res.Probe)
	}
}

func TestExternal_UnchangedWithoutPrevIsError(t *testing.T) {
	script := makeScript(t, `printf '%s\n' '{"unchanged": true}'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatalf("expected error: unchanged with no prev")
	}
}

func TestExternal_TokenMatchShortCircuits(t *testing.T) {
	// Script returns full url+token; refresher should short-circuit because
	// new token == prev.Token.
	script := makeScript(t, `printf '%s\n' '{"url":"https://x","token":"v1"}'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	res, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{URL: "https://x", Token: "v1"}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Unchanged {
		t.Errorf("Unchanged=false, want true (token matches prev)")
	}
}

func TestExternal_PrevPipedAsJSONOnStdin(t *testing.T) {
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.json")
	script := makeScriptIn(t, dir, `cat > `+stdinPath+`; printf '{"url":"https://x","token":"v1"}\n'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	if _, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{URL: "https://prev", Token: "tprev"}}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	got, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	want := `{"prev":{"url":"https://prev","token":"tprev"}}`
	if string(got) != want {
		t.Errorf("script saw stdin %q, want %q", got, want)
	}
}

func TestExternal_ColdStartSendsEmptyObjectOnStdin(t *testing.T) {
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.json")
	script := makeScriptIn(t, dir, `cat > `+stdinPath+`; printf '{"url":"https://x","token":"v1"}\n'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	if _, err := d.Probe(context.Background(), ProbeInput{}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	got, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("cold-start stdin = %q, want {}", got)
	}
}

func TestExternal_NonZeroExitIsErrorWithStderr(t *testing.T) {
	script := makeScript(t, `echo "boom from stderr" >&2; exit 7`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatalf("expected error on exit 7")
	}
	if !strings.Contains(err.Error(), "exit 7") {
		t.Errorf("error %q does not include exit code", err)
	}
	if !strings.Contains(err.Error(), "boom from stderr") {
		t.Errorf("error %q does not include stderr tail", err)
	}
}

func TestExternal_TimeoutHardKills(t *testing.T) {
	script := makeScript(t, `sleep 5`)
	d, _ := NewExternalDiscoverer([]string{script}, 100*time.Millisecond, nil, "src", nil)
	start := time.Now()
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error %q does not mention timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %s, expected hard kill near 100ms", elapsed)
	}
}

func TestExternal_MalformedJSONIsError(t *testing.T) {
	script := makeScript(t, `printf 'not json\n'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

func TestExternal_MissingFieldsIsError(t *testing.T) {
	script := makeScript(t, `printf '{"url":"only-url"}\n'`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	_, err := d.Probe(context.Background(), ProbeInput{})
	if err == nil {
		t.Fatalf("expected error when token is missing")
	}
	if !strings.Contains(err.Error(), "url and token") {
		t.Errorf("error %q does not mention missing fields", err)
	}
}

func TestExternal_PerSourceEnvIsForwardedAndOthersStripped(t *testing.T) {
	// Pick an env name that /bin/sh definitely won't synthesize on its own
	// (PATH and IFS are auto-set by sh when missing).
	t.Setenv("SIGNPOST_TEST_LEAK", "should-not-leak")
	script := makeScript(t, `printf '{"url":"https://x","token":"%s/%s"}\n' "${MY_SECRET:-unset}" "${SIGNPOST_TEST_LEAK:-unset}"`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second,
		map[string]string{"MY_SECRET": "shh"}, "src", nil)
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "shh/unset" {
		t.Errorf("token = %q, want shh/unset (per-source env present, host env stripped)", res.Probe.Token)
	}
}

func TestExternal_AllowlistedProxyEnvForwarded(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.test:3128")
	script := makeScript(t, `printf '{"url":"https://x","token":"%s"}\n' "${HTTPS_PROXY:-unset}"`)
	d, _ := NewExternalDiscoverer([]string{script}, 5*time.Second, nil, "src", nil)
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "http://proxy.test:3128" {
		t.Errorf("token = %q, want HTTPS_PROXY value forwarded", res.Probe.Token)
	}
}
