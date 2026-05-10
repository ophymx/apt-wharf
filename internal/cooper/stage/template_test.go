package stage

import (
	"strings"
	"testing"
)

func TestRenderTemplate_HappyPath(t *testing.T) {
	out, err := RenderTemplate("hugo.service", []byte(
		`Name={{ .Name }} Version={{ .Version }} Arch={{ .Arch }} Epoch={{ .Epoch }} At={{ .PublishedAt }}`),
		Vars{Name: "hugo", Version: "0.140.0", Arch: "amd64", Epoch: 0, PublishedAt: "2026-05-09T08:12:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	want := "Name=hugo Version=0.140.0 Arch=amd64 Epoch=0 At=2026-05-09T08:12:00Z"
	if string(out) != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestRenderTemplate_UnknownFieldErrors(t *testing.T) {
	_, err := RenderTemplate("typo", []byte(`{{ .Versoin }}`), PlaceholderVars())
	if err == nil {
		t.Fatal("expected error on unknown field")
	}
	if !strings.Contains(err.Error(), "Versoin") {
		t.Errorf("error should name the bad field: %v", err)
	}
}

func TestRenderTemplate_ParseError(t *testing.T) {
	_, err := RenderTemplate("bad", []byte(`{{ .Name `), PlaceholderVars())
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestPlaceholderVars_Stable(t *testing.T) {
	v := PlaceholderVars()
	if v.Name == "" || v.Version == "" || v.Arch == "" || v.PublishedAt == "" {
		t.Errorf("placeholder vars should be non-empty: %+v", v)
	}
}

// TestRenderTemplate_NoFuncMap proves the template engine doesn't expose
// helpers like `now` (or anything else from the default Sprig surface).
func TestRenderTemplate_NoFuncMap(t *testing.T) {
	_, err := RenderTemplate("nofunc", []byte(`{{ now }}`), PlaceholderVars())
	if err == nil {
		t.Fatal("expected `now` to be undefined")
	}
}
