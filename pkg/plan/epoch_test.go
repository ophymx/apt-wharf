package plan

import (
	"testing"
)

func TestDeriveEpoch_Deterministic(t *testing.T) {
	a := DeriveEpoch([]byte("https://example.invalid/x:1.2.3"))
	b := DeriveEpoch([]byte("https://example.invalid/x:1.2.3"))
	if a != b {
		t.Errorf("not deterministic: %d vs %d", a, b)
	}
}

func TestDeriveEpoch_DifferentInputsDiffer(t *testing.T) {
	a := DeriveEpoch([]byte("https://example.invalid/x:1.2.3"))
	b := DeriveEpoch([]byte("https://example.invalid/x:1.2.4"))
	if a == b {
		t.Errorf("epoch collision on different input: %d == %d", a, b)
	}
}

func TestDeriveEpoch_FixedWindow(t *testing.T) {
	// All outputs must land in the 2020-01-01 + ~10y window so `ar tv`
	// listings stay readable. Probe a handful of inputs.
	const base = int64(1577836800)
	const span = int64(10 * 365 * 24 * 3600)
	for _, in := range [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("https://example.invalid/x:1.2.3"),
		[]byte("./discover.sh:9.9.9"),
		make([]byte, 4096), // larger input
	} {
		got := DeriveEpoch(in)
		if got < base || got >= base+span {
			t.Errorf("DeriveEpoch(%q) = %d, outside [%d, %d)", in, got, base, base+span)
		}
	}
}

func TestDeriveEpoch_EmptyInput(t *testing.T) {
	// Edge case: empty input is still deterministic and lands in window.
	a := DeriveEpoch(nil)
	b := DeriveEpoch([]byte{})
	if a != b {
		t.Errorf("nil and empty slice should produce same epoch: %d vs %d", a, b)
	}
}
