package cli

import (
	"io"
	"slices"
	"testing"

	"github.com/spf13/pflag"

	"github.com/sofired/treerig/internal/commands"
)

// The removal gate strips -v/--verbose and --progress from the argv it hands
// a pinned teardown binary, and must never take a flag's value for one of
// them. It cannot see this package's flag set, so its list of the flags of
// `worktree remove` that take a value is kept by hand; this keeps it honest.
func TestPinnedChildValueFlagsMatchWorktreeRemove(t *testing.T) {
	root := buildProgram("treerig", "0.0.0", &program{exitCode: new(int)}, io.Discard, io.Discard)
	remove, _, err := root.Find([]string{"worktree", "remove"})
	if err != nil || remove.Name() != "remove" {
		t.Fatalf("worktree remove not found: %v", err)
	}
	// What the gate strips, rather than steps over.
	stripped := map[string]bool{"--" + verboseFlag: true, "--" + progressFlag: true}

	var want []string
	check := func(f *pflag.Flag) {
		name := "--" + f.Name
		if f.NoOptDefVal != "" || stripped[name] {
			return
		}
		want = append(want, name)
		// The gate reads only long names: a short form of a flag that takes a
		// value would get past it.
		if f.Shorthand != "" {
			t.Errorf("%s takes a value and has the short form -%s, which the gate does not read", name, f.Shorthand)
		}
	}
	remove.LocalFlags().VisitAll(check)
	remove.InheritedFlags().VisitAll(check)
	slices.Sort(want)

	if got := commands.PinnedChildValueFlags(); !slices.Equal(got, want) {
		t.Fatalf("the gate steps over the values of %q, but the flags of worktree remove that take one are %q", got, want)
	}
}
