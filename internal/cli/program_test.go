package cli

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/run/osrun"
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
	for _, name := range []string{"shared", "router", "config", "teardown", "completion", "help"} {
		if !strings.Contains(all, "\n  "+name+" ") {
			t.Errorf("help --all does not list %q:\n%s", name, all)
		}
	}
	if !strings.Contains(all, "\nMore\n") || strings.Contains(all, "help --all") {
		t.Errorf("help --all should show More and not advertise itself:\n%s", all)
	}
	// Everything a user needs today is on the default screen.
	for _, name := range []string{"init", "up", "dev", "down", "status", "list", "worktree", "run", "backup", "restore", "route", "doctor"} {
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
// something: no group, no help, no completion, and no hidden stub of a removed
// command (it parses no flags at all, so it declares none).
func leafPaths(root *cobra.Command) [][]string {
	var paths [][]string
	visitCommands(root, func(cmd *cobra.Command) {
		// Cobra's help and completion commands are not this tree's to declare flags on.
		if cmd == root || cmd.HasSubCommands() || cmd.DisableFlagParsing || cmd.Name() == "help" || strings.HasPrefix(cmd.CommandPath(), "treerig completion") || strings.HasPrefix(cmd.Name(), "__") {
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
	// A hand-built Env is not on a terminal, which is plain.
	if env.Verbose || env.Progress != "plain" {
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

// A removed command is a hidden stub that names its replacement and exits 1,
// whatever it is given, flags included.
func TestRemovedCommandStubs(t *testing.T) {
	stubs := []struct {
		path        []string
		replacement string
	}{
		{[]string{"urls"}, "status"},
		{[]string{"index"}, "list --all"},
		{[]string{"worktree", "list"}, "list"},
	}
	for _, stub := range stubs {
		path := strings.Join(stub.path, " ")
		pointer := func(name string) string {
			return "'" + name + " " + path + "' is now '" + name + " " + stub.replacement + "'."
		}
		t.Run(path, func(t *testing.T) {
			for _, extra := range [][]string{nil, {"--json"}, {"--help"}, {"-v"}, {"--config", "x.toml"}, {"--all", "--refresh"}, {"somewhere", "--unknown"}} {
				args := append(append([]string{"treerig"}, stub.path...), extra...)
				var stdout, stderr bytes.Buffer
				code := Main(args, strings.NewReader(""), &stdout, &stderr, func(string) (string, bool) { return "", false })
				if code != 1 || stdout.Len() != 0 || stderr.String() != "[treerig] ERROR: "+pointer("treerig")+"\n" {
					t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
				}
			}

			t.Run("it names the command it was invoked as", func(t *testing.T) {
				var stderr bytes.Buffer
				code := Main(append([]string{"trig"}, stub.path...), strings.NewReader(""), io.Discard, &stderr, func(string) (string, bool) { return "", false })
				if code != 1 || stderr.String() != "[trig] ERROR: "+pointer("trig")+"\n" {
					t.Errorf("exit %d, stderr %q", code, stderr.String())
				}
			})

			t.Run("help about it gives the pointer too", func(t *testing.T) {
				var stdout bytes.Buffer
				code := Main(append([]string{"treerig", "help"}, stub.path...), strings.NewReader(""), &stdout, io.Discard, func(string) (string, bool) { return "", false })
				if code != 0 || !strings.HasPrefix(stdout.String(), pointer("treerig")+"\n") {
					t.Errorf("exit %d, stdout %q", code, stdout.String())
				}
			})

			t.Run("it is hidden from help and from the groups", func(t *testing.T) {
				root, _ := testProgram()
				cmd, _, err := root.Find(stub.path)
				if err != nil || !cmd.Hidden || !cmd.DisableFlagParsing || cmd.GroupID != "" {
					t.Fatalf("stub = %+v, %v", cmd, err)
				}
				for _, all := range []bool{false, true} {
					help := renderHelp(root, 80, all)
					if len(stub.path) > 1 {
						group, _, _ := root.Find(stub.path[:1])
						help = renderHelp(group, 80, all)
					}
					if strings.Contains(help, "\n  "+stub.path[len(stub.path)-1]+" ") {
						t.Errorf("help (all = %v) lists the stub:\n%s", all, help)
					}
				}
			})
		})
	}
}

// `ls` is the short form of `list`, and `list` reads treerig.toml only when
// there is one.
func TestListCommand(t *testing.T) {
	root, _ := testProgram()
	list, _, err := root.Find([]string{"ls"})
	if err != nil || list.Name() != "list" || list.GroupID != dayGroup {
		t.Fatalf("ls = %+v, %v", list, err)
	}
	for _, name := range []string{"all", "refresh", "json", configFlag} {
		if list.Flags().Lookup(name) == nil {
			t.Errorf("list has no --%s", name)
		}
	}
	if usage := list.Flags().Lookup(configFlag).Usage; !strings.Contains(usage, "if any") {
		t.Errorf("--config says %q, want it to say the file is optional", usage)
	}
	if help := renderHelp(list, 80, false); !strings.Contains(help, "Aliases:\n  list, ls\n") {
		t.Errorf("help does not name the alias:\n%s", help)
	}
}

func TestDecideProgress(t *testing.T) {
	getenv := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	}
	for _, tc := range []struct {
		name      string
		requested commands.ProgressMode
		env       map[string]string
		tty       bool
		want      commands.ProgressMode
	}{
		{"auto on a terminal stays auto", commands.ProgressAuto, map[string]string{"TERM": "xterm-256color"}, true, commands.ProgressAuto},
		{"CI set is plain", commands.ProgressAuto, map[string]string{"CI": "true"}, true, commands.ProgressPlain},
		{"CI set to empty is not CI", commands.ProgressAuto, map[string]string{"CI": ""}, true, commands.ProgressAuto},
		{"stderr that is not a terminal is plain", commands.ProgressAuto, nil, false, commands.ProgressPlain},
		{"TERM=dumb is plain", commands.ProgressAuto, map[string]string{"TERM": "dumb"}, true, commands.ProgressPlain},
		{"--progress=plain is plain", commands.ProgressPlain, nil, true, commands.ProgressPlain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideProgress(tc.requested, getenv(tc.env), tc.tty); got != tc.want {
				t.Fatalf("decideProgress = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("reads nothing from a missing environment", func(t *testing.T) {
		if got := decideProgress(commands.ProgressAuto, nil, true); got != commands.ProgressAuto {
			t.Fatalf("decideProgress = %q", got)
		}
	})
}

func TestVerboseReachesTheLoggerAndTheRunner(t *testing.T) {
	build := func(console *logger.Console, runner run.Runner) *program {
		return &program{verbosity: console, buildEnv: func(string) (*commands.Env, error) {
			return &commands.Env{Logger: console, Runner: runner}, nil
		}}
	}
	dockerCall := func(env *commands.Env) {
		// What docker.Run builds: the docker command, with the locks inherited.
		_, _ = env.Runner.Run(context.Background(), run.Spec{
			Command: "docker", Args: []string{"compose", "--project-name", "p", "down", "--", "TOKEN=abc"}, InheritLocks: true,
			Env: []string{"SECRET=env-value"},
		})
		_, _ = env.Runner.Run(context.Background(), run.Spec{Command: "./hook.sh", Args: []string{"--token=abc"}})
	}

	t.Run("with -v each docker command is said, with treerig's arguments only", func(t *testing.T) {
		var stderr bytes.Buffer
		root, _ := testProgram()
		status, _, _ := root.Find([]string{"status"})
		if err := status.ParseFlags([]string{"-v"}); err != nil {
			t.Fatal(err)
		}
		env, err := build(logger.NewConsole("treerig", io.Discard, &stderr), &fakerun.Runner{}).env(status)
		if err != nil {
			t.Fatal(err)
		}
		dockerCall(env)
		if want := "[treerig] docker compose --project-name p down -- ...\n"; stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	})

	t.Run("without -v nothing is said", func(t *testing.T) {
		var stderr bytes.Buffer
		root, _ := testProgram()
		status, _, _ := root.Find([]string{"status"})
		if err := status.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		env, err := build(logger.NewConsole("treerig", io.Discard, &stderr), &fakerun.Runner{}).env(status)
		if err != nil {
			t.Fatal(err)
		}
		dockerCall(env)
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

// TestTracedRunnerStillQuiesces is -v not costing a mutation its proof that
// every child was reaped: lifecycle asks the runner for Quiesce.
func TestTracedRunnerStillQuiesces(t *testing.T) {
	traced := docker.Traced(osrun.New(), func(string) {})
	q, ok := traced.(lifecycle.Quiescer)
	if !ok {
		t.Fatal("the traced runner is not a lifecycle.Quiescer")
	}
	if err := q.Quiesce(context.Background()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
}
