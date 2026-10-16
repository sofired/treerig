package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

func TestHelpWrapsToWidth(t *testing.T) {
	root := buildProgram("treerig", "0.0.0", &program{exitCode: new(int)}, io.Discard, io.Discard)
	for _, width := range []int{80, 100} {
		visitCommands(root, func(cmd *cobra.Command) {
			if cmd.Hidden {
				return
			}
			cmd.InitDefaultHelpFlag()
			for _, line := range strings.Split(renderHelp(cmd, width), "\n") {
				if n := utf8.RuneCountInString(line); n > width {
					t.Errorf("%q help at width %d has a %d-column line:\n%s", cmd.CommandPath(), width, n, line)
				}
			}
		})
	}
}

// With nothing to wrap, renderHelp is exactly cobra's own help, so a
// section cobra would add for a new Aliases, Example or command group, which
// renderHelp does not draw, fails here instead of vanishing from help.
func TestRenderHelpMatchesCobraUnwrapped(t *testing.T) {
	root := buildProgram("treerig", "0.0.0", &program{exitCode: new(int)}, io.Discard, io.Discard)
	cobraHelp := (&cobra.Command{}).HelpFunc()
	visitCommands(root, func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		cmd.InitDefaultHelpFlag()
		var want bytes.Buffer
		cmd.SetOut(&want)
		cobraHelp(cmd, nil)
		if got := renderHelp(cmd, 1<<20); got != want.String() {
			t.Errorf("%q help differs from cobra's:\n--- cobra\n%s--- renderHelp\n%s", cmd.CommandPath(), want.String(), got)
		}
	})
}

func TestWrapIndented(t *testing.T) {
	for _, c := range []struct {
		name          string
		text          string
		width, indent int
		want          string
	}{
		{"fits", "short text", 40, 4, "short text"},
		{"wraps with hanging indent", "one two three four five six seven eight nine ten eleven", 34, 4,
			"one two three four five six\n    seven eight nine ten eleven"},
		{"keeps line breaks", "first line\nsecond line", 40, 2, "first line\n  second line"},
		{"keeps indented lines as they are", "load it with:\n\tsource <(treerig completion bash)   # now", 30, 0,
			"load it with:\n\tsource <(treerig completion bash)   # now"},
		{"short text stays in a narrow room", "one two", 30, 10, "one two"},
		{"long text moves below a narrow room", "one two three four five six seven", 30, 10,
			"\n      one two three four five\n      six seven"},
		{"too narrow to wrap", "one two three four five six seven", 20, 4, "one two three four five six seven"},
		{"never splits a word", "a supercalifragilisticexpialidocious word", 24, 0,
			"a\nsupercalifragilisticexpialidocious\nword"},
	} {
		if got := wrapIndented(c.text, c.width, c.indent); got != c.want {
			t.Errorf("%s: wrapIndented = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWrapFlagUsages(t *testing.T) {
	usages := "      --domain string   narrow a slug registered under more than one domain\n" +
		"  -h, --help            help for remove\n"
	want := "      --domain string   narrow a slug registered under more than\n" +
		"                        one domain\n" +
		"  -h, --help            help for remove"
	if got := wrapFlagUsages(usages, 64); got != want {
		t.Errorf("wrapFlagUsages =\n%s\nwant\n%s", got, want)
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"help", "help", 0},
		{"hepl", "help", 2},
		{"domai", "domain", 1},
		{"kitten", "sitting", 3},
	} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
