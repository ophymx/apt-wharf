package config

import (
	"strings"
	"testing"
)

func TestValidateRepository_AcceptsURLs(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		wantPrefix string
	}{
		{"host only", "https://apt.example.com", ""},
		{"host with port", "https://apt.example.com:8443", ""},
		{"single path segment", "https://apt.example.com/apt", "/apt"},
		{"nested path", "https://host.example/team/apt", "/team/apt"},
		{"http scheme", "http://localhost:8080/apt", "/apt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Repository{Origin: "O", Label: "L", BaseURL: tc.baseURL}
			if err := validateRepository(r); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := r.PathPrefix(); got != tc.wantPrefix {
				t.Errorf("PathPrefix() = %q, want %q", got, tc.wantPrefix)
			}
		})
	}
}

func TestValidateRepository_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"trailing slash on host", "https://apt.example.com/", "trailing slash"},
		{"trailing slash on path", "https://apt.example.com/apt/", "trailing slash"},
		{"non-http scheme", "ftp://apt.example.com", "http or https"},
		{"missing host", "https:///apt", "missing host"},
		{"query string", "https://apt.example.com/apt?x=1", "query or fragment"},
		{"fragment", "https://apt.example.com/apt#foo", "query or fragment"},
		{"dotdot in path", "https://apt.example.com/a/../b", "already-clean"},
		{"duplicate slash in path", "https://apt.example.com/a//b", "already-clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRepository(&Repository{Origin: "O", Label: "L", BaseURL: tc.baseURL})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
