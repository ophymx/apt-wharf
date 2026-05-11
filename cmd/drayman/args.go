package main

import (
	"flag"
	"strings"
)

// boolFlag matches stdlib flag's unexported boolFlag interface
// structurally. Bool flags don't consume a following arg as their
// value, so the reorderer needs to know which flags are bools.
type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

// reorderArgs moves positional arguments after flag arguments so that
// stdlib flag.Parse — which stops at the first non-flag token — sees
// every flag the user passed, regardless of where they put the
// positional. Lets `drayman peek <PLAN> --backend ...` work in
// addition to the strict `drayman peek --backend ... <PLAN>`.
//
// Args after a literal `--` are treated as positionals verbatim and
// preserve their order, mirroring stdlib flag's behavior.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// `--name=value` and `-name=value` carry the value inline.
		name := strings.TrimLeft(a, "-")
		if strings.IndexByte(name, '=') >= 0 {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag — let flag.Parse report it.
			continue
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		// Non-bool flag with a separately-supplied value.
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}
