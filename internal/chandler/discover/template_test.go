package discover

import (
	"strings"
	"testing"
)

func TestExpand_Literal(t *testing.T) {
	got, err := expand("stable", vars{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "stable" {
		t.Errorf("want %q, got %q", "stable", got)
	}
}

func TestExpand_Codename(t *testing.T) {
	got, err := expand("{{.Codename}}-pgdg", vars{Codename: "bookworm"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "bookworm-pgdg" {
		t.Errorf("want %q, got %q", "bookworm-pgdg", got)
	}
}

func TestExpand_Distro(t *testing.T) {
	got, err := expand("linux/{{.Distro}}", vars{Distro: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "linux/ubuntu" {
		t.Errorf("want %q, got %q", "linux/ubuntu", got)
	}
}

func TestExpand_UnknownVariable(t *testing.T) {
	_, err := expand("{{.Nonsense}}", vars{Codename: "bookworm"})
	if err == nil {
		t.Fatal("expected error for unknown variable")
	}
}

func TestExpand_ParseError(t *testing.T) {
	_, err := expand("{{.Codename", vars{})
	if err == nil {
		t.Fatal("expected parse error on unclosed template")
	}
	if !strings.Contains(err.Error(), "parse template") {
		t.Errorf("error should mention parse failure, got %v", err)
	}
}

func TestExpandList_NilStaysNil(t *testing.T) {
	got, err := expandList(nil, vars{})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("nil input should yield nil output, got %v", got)
	}
}

func TestExpandList_PreservesOrder(t *testing.T) {
	got, err := expandList(
		[]string{"a-{{.Codename}}", "b-{{.Codename}}", "c"},
		vars{Codename: "bookworm"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a-bookworm", "b-bookworm", "c"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: want %q got %q", i, want[i], got[i])
		}
	}
}
