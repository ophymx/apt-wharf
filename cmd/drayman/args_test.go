package main

import (
	"flag"
	"reflect"
	"testing"
)

func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("backend", "", "")
	fs.String("distribution", "", "")
	fs.Bool("dry-run", false, "")
	return fs
}

func TestReorderArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "flags only",
			in:   []string{"--backend", "aptly", "--distribution", "stable"},
			want: []string{"--backend", "aptly", "--distribution", "stable"},
		},
		{
			name: "positional last",
			in:   []string{"--backend", "aptly", "PLAN"},
			want: []string{"--backend", "aptly", "PLAN"},
		},
		{
			name: "positional first",
			in:   []string{"PLAN", "--backend", "aptly"},
			want: []string{"--backend", "aptly", "PLAN"},
		},
		{
			name: "positional in middle",
			in:   []string{"--backend", "aptly", "PLAN", "--distribution", "stable"},
			want: []string{"--backend", "aptly", "--distribution", "stable", "PLAN"},
		},
		{
			name: "bool flag does not consume next arg",
			in:   []string{"--dry-run", "PLAN"},
			want: []string{"--dry-run", "PLAN"},
		},
		{
			name: "bool flag in middle",
			in:   []string{"PLAN", "--dry-run", "--backend", "aptly"},
			want: []string{"--dry-run", "--backend", "aptly", "PLAN"},
		},
		{
			name: "stdin sentinel is positional",
			in:   []string{"-", "--backend", "aptly"},
			want: []string{"--backend", "aptly", "-"},
		},
		{
			name: "equals form does not consume next arg",
			in:   []string{"--backend=aptly", "PLAN"},
			want: []string{"--backend=aptly", "PLAN"},
		},
		{
			name: "double-dash separator preserves trailing positionals",
			in:   []string{"--backend", "aptly", "--", "--weird-name"},
			want: []string{"--backend", "aptly", "--weird-name"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderArgs(newFlagSet(), c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
