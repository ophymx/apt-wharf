package plan

import (
	"testing"
)

func TestCanonicalize_SortsKeys(t *testing.T) {
	got, err := Canonicalize(map[string]int{"b": 2, "a": 1, "c": 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":1,"b":2,"c":3}`
	if string(got) != want {
		t.Errorf("Canonicalize sort: got %s, want %s", got, want)
	}
}

func TestCanonicalize_NestedSorts(t *testing.T) {
	in := map[string]any{
		"z": map[string]int{"y": 1, "x": 2},
		"a": []int{3, 1, 2}, // arrays preserve order
	}
	got, err := Canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[3,1,2],"z":{"x":2,"y":1}}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalizeJSON_PassThroughRaw(t *testing.T) {
	got, err := CanonicalizeJSON([]byte(`{"b": 1,    "a":     2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2,"b":1}` {
		t.Errorf("got %s", got)
	}
}
