package external

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadInput_EmptyIsColdStart(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n"} {
		got, err := ReadInput(strings.NewReader(in))
		if err != nil {
			t.Fatalf("ReadInput(%q): %v", in, err)
		}
		if got.Prev != nil {
			t.Errorf("ReadInput(%q).Prev = %+v, want nil", in, got.Prev)
		}
	}
}

func TestReadInput_EmptyObjectIsColdStart(t *testing.T) {
	got, err := ReadInput(strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if got.Prev != nil {
		t.Errorf("Prev = %+v, want nil", got.Prev)
	}
}

func TestReadInput_PrevRoundTrips(t *testing.T) {
	got, err := ReadInput(strings.NewReader(`{"prev":{"url":"https://x","token":"v1"}}`))
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if got.Prev == nil {
		t.Fatal("Prev is nil, want populated")
	}
	if got.Prev.URL != "https://x" || got.Prev.Token != "v1" {
		t.Errorf("Prev = %+v, want url=https://x token=v1", got.Prev)
	}
}

func TestReadInput_MalformedRejected(t *testing.T) {
	if _, err := ReadInput(strings.NewReader("{not json")); err == nil {
		t.Fatal("expected error on malformed input")
	}
}

func TestOutputValidate(t *testing.T) {
	cases := []struct {
		name    string
		out     Output
		wantErr bool
	}{
		{"valid update", Output{URL: "https://x", Token: "v1"}, false},
		{"valid unchanged", Output{Unchanged: true}, false},
		{"unchanged with extras", Output{Unchanged: true, URL: "x"}, true},
		{"unchanged with token only", Output{Unchanged: true, Token: "v1"}, true},
		{"missing token", Output{URL: "https://x"}, true},
		{"missing url", Output{Token: "v1"}, true},
		{"empty", Output{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.out.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate(%+v) err=%v, wantErr=%v", tc.out, err, tc.wantErr)
			}
		})
	}
}

func TestWriteOutput_EmitsOneJSONLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteOutput(&buf, Output{URL: "https://x", Token: "v1"}); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	got := buf.String()
	want := `{"url":"https://x","token":"v1"}` + "\n"
	if got != want {
		t.Errorf("WriteOutput = %q, want %q", got, want)
	}
}

func TestWriteOutput_UnchangedOmitsURLAndToken(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteOutput(&buf, Output{Unchanged: true}); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	if buf.String() != `{"unchanged":true}`+"\n" {
		t.Errorf("WriteOutput = %q", buf.String())
	}
}

func TestWriteOutput_RejectsInvalid(t *testing.T) {
	if err := WriteOutput(&bytes.Buffer{}, Output{}); err == nil {
		t.Fatal("expected validation error on empty Output")
	}
}

func TestRunWith_ColdStart(t *testing.T) {
	var stdout bytes.Buffer
	err := RunWith(context.Background(), strings.NewReader(""), &stdout,
		func(_ context.Context, prev *Probe) (Output, error) {
			if prev != nil {
				t.Errorf("prev = %+v, want nil on cold start", prev)
			}
			return Output{URL: "https://x", Token: "v1"}, nil
		})
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if !strings.Contains(stdout.String(), `"token":"v1"`) {
		t.Errorf("stdout = %q, want token v1", stdout.String())
	}
}

func TestRunWith_PassesPrev(t *testing.T) {
	var seen *Probe
	err := RunWith(context.Background(),
		strings.NewReader(`{"prev":{"url":"https://prev","token":"tprev"}}`),
		&bytes.Buffer{},
		func(_ context.Context, prev *Probe) (Output, error) {
			seen = prev
			return Output{Unchanged: true}, nil
		})
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if seen == nil || seen.Token != "tprev" {
		t.Errorf("prev = %+v, want token tprev", seen)
	}
}

func TestRunWith_PropagatesProbeError(t *testing.T) {
	want := errors.New("upstream sad")
	err := RunWith(context.Background(), strings.NewReader(""), &bytes.Buffer{},
		func(_ context.Context, _ *Probe) (Output, error) {
			return Output{}, want
		})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestRunWith_RejectsInvalidProbeOutput(t *testing.T) {
	err := RunWith(context.Background(), strings.NewReader(""), &bytes.Buffer{},
		func(_ context.Context, _ *Probe) (Output, error) {
			return Output{URL: "missing-token"}, nil
		})
	if err == nil {
		t.Fatal("expected validation error from WriteOutput")
	}
}
