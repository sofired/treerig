package cli

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/sofired/treerig/internal/commands"
)

func TestHelpWrapsToWidth(t *testing.T) {
	root := buildProgram("treerig", "0.0.0", &program{exitCode: new(int)}, io.Discard, io.Discard)
	for _, width := range []int{80, 100} {
		visitCommands(root, func(cmd *cobra.Command) {
			if cmd.Hidden {
				return
			}
			cmd.InitDefaultHelpFlag()
			for _, line := range strings.Split(renderHelp(cmd, width, true), "\n") {
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
		if got := renderHelp(cmd, 1<<20, true); got != want.String() {
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

// testProgram is the command tree with an Env builder that records nothing
// and reads nothing, for tests of parsing and help.
func testProgram() (*cobra.Command, *program) {
	p := &program{
		buildEnv: func(configPath string) (*commands.Env, error) { return &commands.Env{ConfigPath: configPath}, nil },
		exitCode: new(int),
	}
	return buildProgram("treerig", "0.0.0", p, io.Discard, io.Discard), p
}

func TestRootHelpIsGrouped(t *testing.T) {
	root, _ := testProgram()
	root.InitDefaultHelpFlag()
	root.InitDefaultHelpCmd()

	def := renderHelp(root, 80, false)
	all := renderHelp(root, 80, true)
	for _, title := range []string{"Start here", "Day to day", "Project tasks", "Problems"} {
		if !strings.Contains(def, "\n"+title+"\n") {
			t.Errorf("default screen lacks the %q group:\n%s", title, def)
		}
	}
	if strings.Contains(def, "More\n") || strings.Contains(def, "  teardown ") || strings.Contains(def, "  completion ") {
		t.Errorf("default screen shows the More group:\n%s", def)
	}
	if !strings.Contains(def, `Use "treerig help --all" for shared services`) {
		t.Errorf("default screen does not mention help --all:\n%s", def)
	}
	for _, name := range []string{"shared", "router", "config", "teardown", "completion", "index", "help"} {
		if !strings.Contains(all, "\n  "+name+" ") {
			t.Errorf("help --all does not list %q:\n%s", name, all)
		}
	}
	if !strings.Contains(all, "\nMore\n") || strings.Contains(all, "help --all") {
		t.Errorf("help --all should show More and not advertise itself:\n%s", all)
	}
	// Everything a user needs today is on the default screen.
	for _, name := range []string{"init", "up", "dev", "down", "status", "urls", "worktree", "run", "backup", "restore", "route", "doctor"} {
		if !strings.Contains(def, "\n  "+name+" ") {
			t.Errorf("default screen does not list %q:\n%s", name, def)
		}
	}
}

func TestEveryRootCommandHasAGroup(t *testing.T) {
	root, _ := testProgram()
	root.InitDefaultHelpCmd()
	for _, cmd := range root.Commands() {
		// Hidden stubs for removed commands get no group.
		if cmd.GroupID == "" && !cmd.Hidden {
			t.Errorf("%q is in no help group; rootGroups lists it nowhere", cmd.Name())
		}
	}
}

func TestUngroupedCommandsListLast(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	root.AddGroup(&cobra.Group{ID: "a", Title: "Alpha"}, &cobra.Group{ID: moreGroup, Title: "More"})
	root.AddCommand(
		&cobra.Command{Use: "loose", Short: "no group", Run: func(*cobra.Command, []string) {}},
		&cobra.Command{Use: "in-a", Short: "grouped", GroupID: "a", Run: func(*cobra.Command, []string) {}},
		&cobra.Command{Use: "in-more", Short: "hidden", GroupID: moreGroup, Run: func(*cobra.Command, []string) {}},
	)
	got := renderHelp(root, 80, false)
	alpha, loose := strings.Index(got, "Alpha\n  in-a"), strings.Index(got, "Additional Commands:\n  loose")
	if alpha < 0 || loose < alpha {
		t.Errorf("want the Alpha group, then the ungrouped command:\n%s", got)
	}
	if strings.Contains(got, "in-more") {
		t.Errorf("default screen shows a More command:\n%s", got)
	}
	if !strings.Contains(renderHelp(root, 80, true), "More\n  in-more") {
		t.Errorf("all does not show the More group")
	}
}

// leafPaths are the command paths below the root of every command that runs
// something: no group, no help, no completion.
func leafPaths(root *cobra.Command) [][]string {
	var paths [][]string
	visitCommands(root, func(cmd *cobra.Command) {
		// Cobra's help and completion commands are not this tree's to declare flags on.
		if cmd == root || cmd.HasSubCommands() || cmd.Name() == "help" || strings.HasPrefix(cmd.CommandPath(), "treerig completion") || strings.HasPrefix(cmd.Name(), "__") {
			return
		}
		paths = append(paths, strings.Fields(strings.TrimPrefix(cmd.CommandPath(), "treerig ")))
	})
	return paths
}

// TestConfigFlagIsDeclaredPerCommand runs every leaf command with --config,
// its action replaced by a recorder: the commands in configCommands take the
// path, and every other leaf refuses it with the message that says why.
func TestConfigFlagIsDeclaredPerCommand(t *testing.T) {
	reads := map[string]bool{}
	for _, path := range configCommands {
		reads[path] = true
	}
	root, _ := testProgram()
	paths := leafPaths(root)
	if len(paths) == 0 {
		t.Fatal("no leaf commands found")
	}
	for _, path := range paths {
		name := strings.Join(path, " ")
		root, _ := testProgram()
		var got string
		ran := false
		visitCommands(root, func(cmd *cobra.Command) {
			if cmd.Runnable() {
				cmd.Args = nil
				cmd.RunE = func(cmd *cobra.Command, args []string) error {
					ran = true
					got, _ = cmd.Flags().GetString(configFlag)
					return nil
				}
			}
		})
		root.SetArgs(append(slices.Clone(path), "--config", "some.toml"))
		err := root.Execute()
		switch {
		case reads[name]:
			if err != nil || !ran || got != "some.toml" {
				t.Errorf("%q: err = %v, ran = %v, --config = %q; want it to take --config", name, err, ran, got)
			}
		case name == "init":
			want := "'treerig init' writes treerig.toml at the root of the Git repository you run it in; it does not take --config."
			if err == nil || err.Error() != want {
				t.Errorf("%q: err = %v, want %q", name, err, want)
			}
		default:
			want := "'treerig " + name + "' reads no treerig.toml, so it does not take --config."
			if err == nil || err.Error() != want || ran {
				t.Errorf("%q: err = %v, ran = %v, want %q", name, err, ran, want)
			}
		}
	}
	for path := range reads {
		found := false
		for _, p := range paths {
			found = found || strings.Join(p, " ") == path
		}
		if !found {
			t.Errorf("configCommands lists %q, which is no leaf command", path)
		}
	}
	root, _ = testProgram()
	if root.PersistentFlags().Lookup(configFlag) != nil {
		t.Errorf("--config is a persistent flag of the root again")
	}
}

// TestConfigEatsTheNextToken is --config taking whatever follows as its value,
// even "--help".
func TestConfigEatsTheNextToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main([]string{"treerig", "status", "--config", "--help"}, strings.NewReader(""), &stdout, &stderr, func(string) (string, bool) { return "", false })
	got := stderr.String()
	if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(got, "[treerig] ERROR: No treerig.toml at /") || !strings.HasSuffix(got, "/--help.\n") {
		t.Errorf("exit %d, stdout %q, stderr %q; want the missing-file error for a file named --help", code, stdout.String(), got)
	}
}

func TestVerboseProgressAndVersionFlags(t *testing.T) {
	root, p := testProgram()
	status, _, _ := root.Find([]string{"status"})
	if err := status.ParseFlags([]string{"-v", "--progress=plain", "--config", "x.toml"}); err != nil {
		t.Fatal(err)
	}
	env, err := p.env(status)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Verbose || env.Progress != "plain" || env.ConfigPath != "x.toml" {
		t.Errorf("env = verbose %v, progress %q, config %q", env.Verbose, env.Progress, env.ConfigPath)
	}

	status = testProgramFind("status")
	env, _ = (&program{buildEnv: func(string) (*commands.Env, error) { return &commands.Env{}, nil }}).env(status)
	if env.Verbose || env.Progress != "auto" {
		t.Errorf("defaults: verbose %v, progress %q", env.Verbose, env.Progress)
	}

	// -v and -V are different flags, and -V is the root's alone.
	if f := root.Flags().ShorthandLookup("V"); f == nil || f.Name != "version" {
		t.Errorf("-V = %v, want --version", f)
	}
	if f := status.InheritedFlags().ShorthandLookup("v"); f == nil || f.Name != "verbose" {
		t.Errorf("-v = %v, want --verbose", f)
	}
	if status.Flags().ShorthandLookup("V") != nil {
		t.Errorf("-V is on status too")
	}
}

// testProgramFind is the named command of a fresh tree.
func testProgramFind(name string) *cobra.Command {
	root, _ := testProgram()
	cmd, _, _ := root.Find([]string{name})
	return cmd
}

func TestDevTeardownIsHiddenButAccepted(t *testing.T) {
	remove, _, _ := testProgramFind("worktree").Find([]string{"remove"})
	flag := remove.Flags().Lookup("dev-teardown")
	if flag == nil || !flag.Hidden {
		t.Fatalf("--dev-teardown = %+v, want a hidden flag", flag)
	}
	if err := remove.ParseFlags([]string{"--dev-teardown"}); err != nil {
		t.Errorf("--dev-teardown is not accepted: %v", err)
	}
	if strings.Contains(renderHelp(remove, 80, true), "dev-teardown") {
		t.Errorf("help lists --dev-teardown")
	}
}
