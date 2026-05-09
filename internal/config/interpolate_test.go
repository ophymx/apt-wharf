package config

import (
	"strings"
	"testing"
)

func TestExpandEnv_HappyPath(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Setenv("PORT", "8080")

	got, err := ExpandEnv([]byte("listen: :${PORT}\ntoken: ${FOO}\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "listen: :8080\ntoken: bar\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandEnv_MissingVarFailsLoud(t *testing.T) {
	_, err := ExpandEnv([]byte("token: ${DEFINITELY_NOT_SET_XYZ}"))
	if err == nil {
		t.Fatal("expected error for missing var")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_XYZ") {
		t.Fatalf("error did not name the missing var: %v", err)
	}
}

func TestExpandEnv_EmptyVarFailsLoud(t *testing.T) {
	t.Setenv("EMPTY_VAR", "")
	_, err := ExpandEnv([]byte("token: ${EMPTY_VAR}"))
	if err == nil {
		t.Fatal("expected error for empty var")
	}
}

func TestExpandEnv_NoRefsIsNoop(t *testing.T) {
	in := []byte("listen: :8080\n")
	got, err := ExpandEnv(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(in) {
		t.Fatalf("got %q want %q", got, in)
	}
}

func TestExpandEnv_MultipleMissingVarsListed(t *testing.T) {
	_, err := ExpandEnv([]byte("a: ${MISSING_A_XYZ}\nb: ${MISSING_B_XYZ}\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "MISSING_A_XYZ") || !strings.Contains(err.Error(), "MISSING_B_XYZ") {
		t.Fatalf("expected both vars listed, got: %v", err)
	}
}
