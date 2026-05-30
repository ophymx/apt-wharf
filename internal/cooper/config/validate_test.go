package config

import (
	"strings"
	"testing"
)

func TestParseHumanBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  string // substring; empty means "no error"
	}{
		{"1024", 1024, ""},
		{"1024B", 1024, ""},
		{"8K", 8_000, ""},
		{"8KB", 8_000, ""},
		{"8Ki", 8 << 10, ""},
		{"8KiB", 8 << 10, ""},
		{"2M", 2_000_000, ""},
		{"2MiB", 2 << 20, ""},
		{"4G", 4_000_000_000, ""},
		{"8GiB", 8 << 30, ""},
		{"1T", 1_000_000_000_000, ""},
		{"1TiB", 1 << 40, ""},

		// Errors
		{"", 0, "empty"},
		{"  ", 0, "empty"},
		{"abc", 0, "no leading digits"},
		{"8XiB", 0, "unknown unit"},
		{"8 GiB", 0, "unknown unit"}, // embedded whitespace not allowed
		{"8gi", 0, "unknown unit"},   // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseHumanBytes(c.in)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Errorf("err: got %v, want substring %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestValidateExtract_Defaults(t *testing.T) {
	// Nil block: no-op.
	if err := validateExtract(nil); err != nil {
		t.Errorf("nil should be ok, got %v", err)
	}
}

func TestValidateExtract_EmptyBlockRejected(t *testing.T) {
	err := validateExtract(&ExtractLimits{})
	if err == nil || !strings.Contains(err.Error(), "at least one of max_bytes, max_files") {
		t.Errorf("expected empty-block rejection, got %v", err)
	}
}

func TestValidateExtract_HappyPath(t *testing.T) {
	e := &ExtractLimits{MaxBytes: "8GiB", MaxFiles: 50000}
	if err := validateExtract(e); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if e.maxBytesParsed != 8<<30 {
		t.Errorf("maxBytesParsed: %d, want %d", e.maxBytesParsed, int64(8<<30))
	}
	if e.ResolvedMaxBytes() != 8<<30 {
		t.Errorf("ResolvedMaxBytes: %d", e.ResolvedMaxBytes())
	}
	if e.ResolvedMaxFiles() != 50000 {
		t.Errorf("ResolvedMaxFiles: %d", e.ResolvedMaxFiles())
	}
}

func TestValidateExtract_OnlyBytes(t *testing.T) {
	e := &ExtractLimits{MaxBytes: "8GiB"}
	if err := validateExtract(e); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// MaxFiles=0 is the "use default" sentinel; not an error.
	if e.ResolvedMaxFiles() != 0 {
		t.Errorf("ResolvedMaxFiles: %d, want 0 (fall back to default)", e.ResolvedMaxFiles())
	}
}

func TestValidateExtract_OnlyFiles(t *testing.T) {
	e := &ExtractLimits{MaxFiles: 50000}
	if err := validateExtract(e); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if e.ResolvedMaxBytes() != 0 {
		t.Errorf("ResolvedMaxBytes: %d, want 0", e.ResolvedMaxBytes())
	}
}

func TestValidateExtract_BytesAboveCeiling(t *testing.T) {
	e := &ExtractLimits{MaxBytes: "128GiB"} // 128 > 64 GiB ceiling
	err := validateExtract(e)
	if err == nil || !strings.Contains(err.Error(), "exceeds hard ceiling") {
		t.Errorf("expected ceiling error, got %v", err)
	}
}

func TestValidateExtract_FilesAboveCeiling(t *testing.T) {
	e := &ExtractLimits{MaxFiles: 5_000_000}
	err := validateExtract(e)
	if err == nil || !strings.Contains(err.Error(), "exceeds hard ceiling") {
		t.Errorf("expected ceiling error, got %v", err)
	}
}

func TestValidateExtract_UnparseableBytes(t *testing.T) {
	e := &ExtractLimits{MaxBytes: "8 frobnitzes"}
	err := validateExtract(e)
	if err == nil || !strings.Contains(err.Error(), "extract.max_bytes") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestValidateExtract_NegativeFiles(t *testing.T) {
	e := &ExtractLimits{MaxFiles: -1}
	err := validateExtract(e)
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("expected non-negative error, got %v", err)
	}
}

func TestValidateSource_ExternalEnvForward_Happy(t *testing.T) {
	src := &Source{External: &ExternalSource{
		Command:    []string{"./d.sh"},
		EnvForward: []string{"GITHUB_TOKEN", "MY_OTHER_VAR"},
	}}
	if err := validateSource(src, ""); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestValidateSource_ExternalEnvForward_EmptyEntry(t *testing.T) {
	src := &Source{External: &ExternalSource{
		Command:    []string{"./d.sh"},
		EnvForward: []string{"GITHUB_TOKEN", ""},
	}}
	err := validateSource(src, "")
	if err == nil || !strings.Contains(err.Error(), "empty entry") {
		t.Errorf("expected empty-entry error, got %v", err)
	}
}

func TestValidateSource_ExternalEnvForward_EqualsRejected(t *testing.T) {
	src := &Source{External: &ExternalSource{
		Command:    []string{"./d.sh"},
		EnvForward: []string{"FOO=BAR"},
	}}
	err := validateSource(src, "")
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Errorf("expected '=' rejection, got %v", err)
	}
}
