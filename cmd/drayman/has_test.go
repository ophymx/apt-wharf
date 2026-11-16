package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ophymx/apt-wharf/internal/drayman/backend"
)

func hasFixture() *stubBackend {
	be := newStubBackend()
	be.listings["bigapp|amd64"] = []backend.Package{
		{Name: "bigapp", Version: "4.1-repack2", Architecture: "amd64"},
		{Name: "bigapp", Version: "4.2-repack3", Architecture: "amd64"},
	}
	be.listings["bigapp|arm64"] = []backend.Package{
		{Name: "bigapp", Version: "4.2-repack3", Architecture: "arm64"},
	}
	// reprepro shape: one arch:all entry listed once per index.
	be.listings["bigapp-data|all"] = []backend.Package{
		{Name: "bigapp-data", Version: "4.2-repack3", Architecture: "all"},
		{Name: "bigapp-data", Version: "4.2-repack3", Architecture: "all"},
	}
	return be
}

func TestRunHas_PresentWithArch(t *testing.T) {
	var out bytes.Buffer
	if err := runHas(context.Background(), hasFixture(), "bigapp", "4.2-repack3", "amd64", &out); err != nil {
		t.Fatalf("runHas: %v", err)
	}
	if got, want := out.String(), "bigapp\tamd64\tpresent\t4.2-repack3\n"; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
}

func TestRunHas_PresentAnyArch(t *testing.T) {
	var out bytes.Buffer
	if err := runHas(context.Background(), hasFixture(), "bigapp", "4.2-repack3", "", &out); err != nil {
		t.Fatalf("runHas: %v", err)
	}
	want := "bigapp\tamd64\tpresent\t4.2-repack3\nbigapp\tarm64\tpresent\t4.2-repack3\n"
	if got := out.String(); got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}

	out.Reset()
	if err := runHas(context.Background(), hasFixture(), "bigapp-data", "4.2-repack3", "", &out); err != nil {
		t.Fatalf("runHas: %v", err)
	}
	if got, want := out.String(), "bigapp-data\tall\tpresent\t4.2-repack3\n"; got != want {
		t.Errorf("repeated arch:all listing must print once: %q", got)
	}
}

func TestRunHas_MissingIsExit10(t *testing.T) {
	cases := []struct{ name, version, arch, wantRow string }{
		{"bigapp", "4.3-repack3", "", "bigapp\t*\tmissing\t4.3-repack3\n"},          // other versions only
		{"bigapp", "4.1-repack2", "arm64", "bigapp\tarm64\tmissing\t4.1-repack2\n"}, // present, but on amd64 only
		{"nope", "1.0", "", "nope\t*\tmissing\t1.0\n"},                              // no such package
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runHas(context.Background(), hasFixture(), tc.name, tc.version, tc.arch, &out)
		ec, ok := errors.AsType[exitCodeError](err)
		if !ok || ec.code != exitMissing {
			t.Errorf("%s %s %q: err=%v, want exit status %d", tc.name, tc.version, tc.arch, err, exitMissing)
		}
		if out.String() != tc.wantRow {
			t.Errorf("stdout %q, want %q", out.String(), tc.wantRow)
		}
	}
}

func TestRunHas_ComparesAsDebianVersions(t *testing.T) {
	be := newStubBackend()
	be.listings["tool|amd64"] = []backend.Package{{Name: "tool", Version: "0:1.0", Architecture: "amd64"}}
	var out bytes.Buffer
	// dpkg treats an absent epoch as 0.
	if err := runHas(context.Background(), be, "tool", "1.0", "amd64", &out); err != nil {
		t.Fatalf("0:1.0 and 1.0 are the same version to dpkg: %v", err)
	}
}

func TestRunHas_QueryErrorIsNotMissing(t *testing.T) {
	// "Couldn't ask" must never look like "not there": a recipe would
	// rebuild on every backend outage.
	be := hasFixture()
	be.listErr = errors.New("aptly 502")
	var out bytes.Buffer
	err := runHas(context.Background(), be, "bigapp", "4.2-repack3", "", &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := errors.AsType[exitCodeError](err); ok {
		t.Fatalf("query failure must exit 1, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no row expected on error, got %q", out.String())
	}
}
