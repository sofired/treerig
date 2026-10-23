package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
)

// init turns off cobra's alphabetical sorting of subcommands, a
// package-wide setting, so help lists them in the order buildProgram adds
// them: the everyday workflow first. It is set once here, never per call,
// so concurrent Main calls never write it.
func init() {
	cobra.EnableCommandSorting = false
}

// errVersionRequested is root's PersistentPreRunE's own signal that it has
// already printed the version and decided the exit code (0): runMain
// recognises it and returns without going through report() or clierr.Code,
// the same way cobra's own flag.ErrHelp sentinel says "already handled" for
// --help.
var errVersionRequested = errors.New("cli: version requested")

// program is what every command closure buildProgram creates shares: how to
// build the commands.Env once --config is known, and where a leaf that
// decides its own exit code (doctor, router status) records it.
type program struct {
	buildEnv func(configPath string) (*commands.Env, error)
	exitCode *int
}

// env resolves cmd's own --config (declared by addConfigFlag on each command
// that reads treerig.toml; "" on one that does not) and root's persistent
// -v/--verbose and --progress into a commands.Env for cmd's action.
func (p *program) env(cmd *cobra.Command) (*commands.Env, error) {
	configPath, _ := cmd.Flags().GetString(configFlag)
	env, err := p.buildEnv(configPath)
	if err != nil {
		return nil, err
	}
	// Both are the root's persistent flags, which every command's flag set
	// has, so these lookups cannot fail; a hand-built command without them
	// reads as the defaults.
	env.Verbose, _ = cmd.Flags().GetBool(verboseFlag)
	if f := cmd.Flags().Lookup(progressFlag); f != nil {
		env.Progress = commands.ProgressMode(f.Value.String())
	}
	if env.Progress == "" {
		env.Progress = commands.ProgressAuto
	}
	return env, nil
}

// The names of the flags this tree declares in more than one place, or reads
// back from a command's flag set.
const (
	configFlag   = "config"
	verboseFlag  = "verbose"
	progressFlag = "progress"
)

// The help groups of the root command, in the order its help lists them.
// moreGroup is the one the default screen hides and `help --all` shows.
const (
	startGroup    = "start"
	dayGroup      = "day"
	tasksGroup    = "tasks"
	problemsGroup = "problems"
	moreGroup     = "more"
)

// addConfigFlag declares --config on cmd, a command that reads treerig.toml.
// optional says the command also runs without one, which its help then says. It is a flag of that command alone, so help, completion and the unknown
// flag error of a command that reads no configuration (init, router,
// teardown, config schema) all follow from where it is declared.
func addConfigFlag(cmd *cobra.Command, optional bool) {
	usage := fmt.Sprintf("path to %s (default: nearest above cwd)", brand.ConfigFile)
	if optional {
		usage = fmt.Sprintf("path to %s (default: nearest above cwd, if any)", brand.ConfigFile)
	}
	cmd.Flags().String(configFlag, "", usage)
	_ = cmd.MarkFlagFilename(configFlag, "toml")
}

// rootGroups is the help group of each of the root's own commands. It maps
// today's command set onto the groups docs/design/cli-1.0.md names, until
// that design's commands (open, logs, list, remove, rename) exist: the
// everyday commands are on the default screen, and what is not (shared
// services, the router, configuration, the teardown pin, the index,
// completion and help itself) is under More, which `help --all` shows.
// A command missing here is listed last, ungrouped.
var rootGroups = map[string]string{
	"init":       startGroup,
	"up":         startGroup,
	"dev":        dayGroup,
	"down":       dayGroup,
	"status":     dayGroup,
	"urls":       dayGroup,
	"worktree":   dayGroup,
	"run":        tasksGroup,
	"backup":     tasksGroup,
	"restore":    tasksGroup,
	"route":      tasksGroup,
	"doctor":     problemsGroup,
	"config":     moreGroup,
	"index":      moreGroup,
	"router":     moreGroup,
	"shared":     moreGroup,
	"teardown":   moreGroup,
	"completion": moreGroup,
	"help":       moreGroup,
}

// configCommands are the commands, by path below the root, that read
// treerig.toml and so take --config. init, config schema, router and teardown
// read none. The commands in configOptional also run without one.
var configCommands = []string{
	"config validate", "config show",
	"dev", "up", "down", "status", "urls",
	"run", "backup", "restore", "index", "doctor",
	"route add", "route rm", "route ls",
	"shared up", "shared down", "shared status",
	"worktree list", "worktree remove", "worktree rename", "worktree unlock",
}

// configOptional are the configCommands that work without a treerig.toml (they
// use commands.ContextWithoutConfig), so their --config says the default may
// not exist.
var configOptional = []string{"worktree list", "worktree remove", "worktree unlock"}

// declareGroupsAndConfig gives each of the root's commands its help group
// (rootGroups) and each command that reads treerig.toml its --config flag
// (configCommands). A path that names no command is a mistake in the tables
// above, and panics so that any test of the tree finds it.
func declareGroupsAndConfig(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		cmd.GroupID = rootGroups[cmd.Name()]
	}
	for _, path := range configCommands {
		cmd, rest, err := root.Find(strings.Fields(path))
		if err != nil || len(rest) > 0 || cmd == root {
			panic(fmt.Sprintf("cli: configCommands names no command: %q", path))
		}
		addConfigFlag(cmd, slices.Contains(configOptional, path))
	}
}

// progressValue is --progress's pflag.Value: one of commands.ProgressModes,
// "auto" by default, and a usage error for any other. It reports its type as
// "mode", which is the value name help shows (--progress mode).
type progressValue commands.ProgressMode

// String is the mode as given.
func (v *progressValue) String() string { return string(*v) }

// Set accepts one of commands.ProgressModes.
func (v *progressValue) Set(s string) error {
	if !slices.Contains(commands.ProgressModes, commands.ProgressMode(s)) {
		names := make([]string, len(commands.ProgressModes))
		for i, m := range commands.ProgressModes {
			names[i] = string(m)
		}
		return fmt.Errorf("must be one of: %s", strings.Join(names, ", "))
	}
	*v = progressValue(s)
	return nil
}

// Type is the value name help shows after the flag.
func (*progressValue) Type() string { return "mode" }

// exit records the exit code the process should end with when the current
// action returns nil: doctor's own diagnostic code, or router status's
// already-printed failure.
func (p *program) exit(code int) { *p.exitCode = code }

// usageArgs adapts a cobra.PositionalArgs validator so a failure it reports
// is treated the way every other parse-time mistake in this tree is: a
// clierr.Error, which the top-level report() prints as "[name] ERROR: ...",
// exit 1, with no TREERIG_DEBUG hint — rather than the crash report() takes
// anything else reaching it to be.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return clierr.New(err.Error())
		}
		return nil
	}
}

// maxArgs accepts at most n positional arguments, and says which ones were
// too many when it refuses — cobra's own NoArgs calls a stray argument an
// unknown command, which a leaf with no subcommands has never looked for.
func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) <= n {
			return nil
		}
		takes := "no arguments"
		switch {
		case n == 1:
			takes = "at most 1 argument"
		case n > 1:
			takes = fmt.Sprintf("at most %d arguments", n)
		}
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = strconv.Quote(a)
		}
		return clierr.Newf("%q takes %s, got %d: %s", cmd.CommandPath(), takes, len(args), strings.Join(quoted, " "))
	}
}

// exactArgs accepts exactly n positional arguments, and otherwise refuses in
// maxArgs' shape. what names the one argument for the message, when n is 1.
func exactArgs(n int, what string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == n {
			return nil
		}
		takes := fmt.Sprintf("exactly %d arguments", n)
		if n == 1 {
			takes = "exactly 1 argument, " + what
		}
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = strconv.Quote(a)
		}
		got := fmt.Sprintf("got %d", len(args))
		if len(args) > 0 {
			got += ": " + strings.Join(quoted, " ")
		}
		return clierr.Newf("%q takes %s, %s", cmd.CommandPath(), takes, got)
	}
}

// flagError is the whole tree's flag-parse error handler: pflag's own
// message as a clierr.Error, plus a "Did you mean this?" list for an unknown
// long flag, in the same shape unknownSubcommand gives an unknown command
// (pflag, unlike cobra, suggests nothing itself).
func flagError(cmd *cobra.Command, err error) error {
	if refusal := misplacedFlag(cmd, err); refusal != nil {
		return refusal
	}
	msg := err.Error()
	typed, ok := strings.CutPrefix(msg, "unknown flag: --")
	if !ok {
		return clierr.New(msg)
	}
	var suggestions []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && isNear(typed, f.Name) {
			suggestions = append(suggestions, "--"+f.Name)
		}
	})
	if len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return clierr.New(msg)
}

// misplacedFlag is the error for a flag this tree has, but not on cmd: --config
// on a command that reads no treerig.toml, and -V/--version anywhere but the
// root. It is nil for any other flag error, and pflag's bare "unknown flag"
// would say nothing of why these are refused.
func misplacedFlag(cmd *cobra.Command, err error) error {
	root := cmd.Root()
	switch err.Error() {
	case "unknown flag: --config":
		switch {
		case cmd.CommandPath() == root.CommandPath()+" init":
			return clierr.Newf("'%s' writes %s at the root of the Git repository you run it in; it does not take --config.",
				cmd.CommandPath(), brand.ConfigFile)
		case cmd.HasSubCommands():
			return clierr.Newf("'%s' does not take --config itself: give it to the command that reads %s, after that command's name.",
				cmd.CommandPath(), brand.ConfigFile)
		}
		return clierr.Newf("'%s' reads no %s, so it does not take --config.", cmd.CommandPath(), brand.ConfigFile)
	case "unknown shorthand flag: 'V' in -V":
		return clierr.Newf("-V is only accepted on its own: run '%s -V'.", root.CommandPath())
	case "unknown flag: --version":
		return clierr.Newf("--version is only accepted on its own: run '%s --version'.", root.CommandPath())
	}
	return nil
}

// isNear is cobra's own test for suggesting a command name, applied to a
// flag name: within two single-character edits of typed, or starting with
// it, ignoring case.
func isNear(typed, name string) bool {
	typed, name = strings.ToLower(typed), strings.ToLower(name)
	return editDistance(typed, name) <= 2 || strings.HasPrefix(name, typed)
}

// editDistance is the Levenshtein distance between a and b: the fewest
// single-byte insertions, deletions and substitutions that turn one into the
// other.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// unknownSubcommand rejects a positional argument on a command group that
// only ever dispatches to a named child (the root itself, config, router and
// worktree): anything left over names a subcommand that does not exist. It
// reports that the same "unknown command %q for %q" shape, with the same
// "Did you mean this?" suggestions, cobra's own default Args check
// (legacyArgs) gives the root command — but applied at every level, since
// legacyArgs itself only ever reaches the root.
func unknownSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if !cmd.DisableSuggestions {
		if cmd.SuggestionsMinimumDistance <= 0 {
			cmd.SuggestionsMinimumDistance = 2
		}
		if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
			msg += "\n\nDid you mean this?\n"
			for _, s := range suggestions {
				msg += "\t" + s + "\n"
			}
		}
	}
	return errors.New(msg)
}

// bareGroup is a command group's own RunE for when it is invoked with no
// subcommand at all: its own help, to stderr, exit 1 — the same "you have to
// say what you mean" failure a bad subcommand name or option gets, just with
// nothing left to name as the culprit.
func bareGroup(p *program) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		writeHelp(cmd, cmd.ErrOrStderr(), false)
		p.exit(1)
		return nil
	}
}

// newHelpCommand is this tree's own "help [command]" leaf, replacing
// cobra's default one: cobra's own help command calls CheckErr, which can
// exit the process directly on a lookup failure, bypassing this package's
// own exit-code and signal handling entirely. Found, it shows a command's
// own help, to stdout, exit 0 — walking the full path given, not just its
// first word. A path with words that name no command shows the help of the
// deepest command it did name (root's, if none), to stderr, exit 1, the same
// as any other unresolvable name in this tree. With --all, root's help also
// lists the More group.
func newHelpCommand(p *program) *cobra.Command {
	var all bool
	help := &cobra.Command{
		Use:   "help [command]",
		Short: "display help for command",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			target, rest, err := root.Find(args)
			// Find stops at the deepest command the words name and returns
			// the words it could not place, rather than failing: every
			// command here has its own Args validator, so Find leaves
			// unknown words to it. Only a path with nothing left over names
			// a command; bare "help" names root.
			if err != nil {
				target = root
			}
			// Executing a command the normal way is what registers its own
			// -h/--help flag (InitDefaultHelpFlag, called from execute());
			// jumping straight to its help, as this does, has to register
			// it by hand first, the same way cobra's own help command does.
			target.InitDefaultHelpFlag()
			if err != nil || len(rest) > 0 {
				writeHelp(target, root.ErrOrStderr(), all)
				p.exit(1)
				return nil
			}
			writeHelp(target, root.OutOrStdout(), all)
			return nil
		},
	}
	help.Flags().BoolVar(&all, "all", false, "also list the commands the default screen leaves out")
	return help
}

// addCompletion adds cobra's "completion <shell>" command, which prints a
// bash, zsh, fish or PowerShell script that completes this tree's commands
// and flags through cobra's hidden __complete command. Cobra's completion
// commands keep the output writer root has when they are created, so this
// runs after root.SetOut. The group and its shells are then given this
// tree's own conventions: lower-case descriptions, and help to stderr with
// exit 1 when no shell is named.
func addCompletion(root *cobra.Command, p *program) {
	root.InitDefaultCompletionCmd()
	completion, _, err := root.Find([]string{"completion"})
	if err != nil || completion == root {
		return
	}
	completion.Short = "print a shell completion script"
	completion.Long = ""
	completion.Args = usageArgs(unknownSubcommand)
	completion.RunE = bareGroup(p)
	for _, shell := range completion.Commands() {
		shell.Short = fmt.Sprintf("print the completion script for %s", shell.Name())
	}

	// Cobra completes file names wherever it is told nothing else. Only
	// --config (addConfigFlag marks it as a .toml file) names a file; no
	// positional argument and no other flag value in this tree does. Flag
	// completions live in a cobra-wide map, locked for concurrent use, that
	// gains an entry for each flag of each tree.
	visitCommands(root, func(cmd *cobra.Command) {
		if cmd.ValidArgsFunction == nil && len(cmd.ValidArgs) == 0 {
			cmd.ValidArgsFunction = cobra.NoFileCompletions
		}
		cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "string" && f.Name != configFlag {
				_ = cmd.RegisterFlagCompletionFunc(f.Name, cobra.NoFileCompletions)
			}
		})
	})
}

// visitCommands calls visit on cmd and on every command beneath it, parents
// before children.
func visitCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, child := range cmd.Commands() {
		visitCommands(child, visit)
	}
}

// splitRunArgs separates `run`'s arguments into the command name before
// "--" (empty when there is none) and everything after it, which belongs to
// the command. Without a "--", nothing is the command's.
func splitRunArgs(cmd *cobra.Command, args []string) (name string, rest []string) {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return arg0(args), nil
	}
	return arg0(args[:dash]), args[dash:]
}

// runArgs accepts `run`'s arguments: at most the command's name before
// "--". Anything else before it would be silently ambiguous between a
// mistyped name and an argument meant for the command, so it is refused with
// the spelling that says which was meant: the extra arguments moved to just
// after "--", ahead of any already there.
func runArgs(cmd *cobra.Command, args []string) error {
	dash := cmd.ArgsLenAtDash()
	before, after := args, []string(nil)
	if dash >= 0 {
		before, after = args[:dash], args[dash:]
	}
	if len(before) <= 1 {
		return nil
	}
	quoted := make([]string, len(before))
	for i, a := range before {
		quoted[i] = strconv.Quote(a)
	}
	return clierr.Newf("%q takes one command name before \"--\", got %d arguments: %s. Pass the command's own arguments after \"--\", as in: %s",
		cmd.CommandPath(), len(before), strings.Join(quoted, " "), runSuggestion(cmd.CommandPath(), before[0], append(slices.Clone(before[1:]), after...)))
}

// runSuggestion is the spelling that passes rest to the command name:
// "treerig run plan -- extra more", each argument holding whitespace (or
// nothing) in double quotes.
func runSuggestion(path, name string, rest []string) string {
	words := []string{path, name, "--"}
	for _, a := range rest {
		if a == "" || strings.ContainsAny(a, " \t\n") {
			a = strconv.Quote(a)
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// runFlagError is `run`'s flag-parse error handler: flagError's message, and
// for an unknown flag, where the mistake almost always was leaving out "--"
// (`treerig run plan -auto-approve`), the spelling that passes it on.
func runFlagError(cmd *cobra.Command, err error) error {
	if refusal := misplacedFlag(cmd, err); refusal != nil {
		return refusal
	}
	base := flagError(cmd, err)
	msg := err.Error()
	var flag string
	switch {
	case strings.HasPrefix(msg, "unknown flag: "):
		flag = strings.TrimPrefix(msg, "unknown flag: ")
	case strings.HasPrefix(msg, "unknown shorthand flag: "):
		_, flag, _ = strings.Cut(msg, "' in ")
	default:
		return base
	}
	name := "<name>"
	if positional := cmd.Flags().Args(); len(positional) > 0 {
		name = positional[0]
	}
	hint := fmt.Sprintf("Pass the command's own arguments after \"--\", as in: %s", runSuggestion(cmd.CommandPath(), name, []string{flag}))
	head, suggestions, hasSuggestions := strings.Cut(base.Error(), "\n\n")
	out := head + ". " + hint
	if hasSuggestions {
		out += "\n\n" + suggestions
	}
	return clierr.New(out)
}

// arg0 is args[0], or "" if there isn't one: every leaf in this tree that
// takes an optional identifier takes at most one.
func arg0(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// worktreeNameHelp is the last paragraph of the Long text of the commands that take a worktree: the
// forms registry.Registry.Resolve reads, in one sentence.
const worktreeNameHelp = "Name the worktree by its UUID, its host, the path to its directory (./ ../ / ~) or its slug. With no name, the worktree you are in."

// buildProgram assembles this binary's own cobra command tree, verb for
// verb, in the order its help output lists them. A fresh tree is built on
// every call — a cobra.Command's own parsed flag values live on the Command
// itself, so cli.Main running more than once in a process (every in-process
// test does) needs to start clean.
//
// Help is rendered by renderHelp, which wraps every description to the
// terminal, so a Short, Long or flag usage here can be as long as it needs.
func buildProgram(commandName, version string, p *program, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           commandName,
		Short:         "Give every Git worktree a durable identity, an isolated home, and a routed URL.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          usageArgs(unknownSubcommand),
	}
	root.AddGroup(
		&cobra.Group{ID: startGroup, Title: "Start here"},
		&cobra.Group{ID: dayGroup, Title: "Day to day"},
		&cobra.Group{ID: tasksGroup, Title: "Project tasks"},
		&cobra.Group{ID: problemsGroup, Title: "Problems"},
		&cobra.Group{ID: moreGroup, Title: "More"},
	)
	// -V is the root's alone, so no other page lists it; -v is the verbose
	// flag of every command. pflag tells them apart by case.
	root.Flags().BoolP("version", "V", false, "output the version number")
	root.PersistentFlags().BoolP(verboseFlag, "v", false, "show what treerig runs, and every detail")
	progress := progressValue(commands.ProgressAuto)
	root.PersistentFlags().Var(&progress, progressFlag, "how to show progress: auto (redraw on a terminal) or plain (one line per change) (default \"auto\")")
	// pflag quotes a default only for a "string" flag, and this one is named
	// "mode" in help, so the default is written in the usage instead.
	root.PersistentFlags().Lookup(progressFlag).DefValue = ""
	_ = root.RegisterFlagCompletionFunc(progressFlag, cobra.FixedCompletions([]cobra.Completion{
		cobra.CompletionWithDesc(string(commands.ProgressAuto), "redraw on a terminal"),
		cobra.CompletionWithDesc(string(commands.ProgressPlain), "one line per change"),
	}, cobra.ShellCompDirectiveNoFileComp))
	root.SetFlagErrorFunc(flagError)
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Only the root has -V; no other command's flag set has "version".
		if cmd != cmd.Root() {
			return nil
		}
		if v, _ := cmd.Flags().GetBool("version"); v {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return errVersionRequested
		}
		return nil
	}
	root.RunE = bareGroup(p)
	help := newHelpCommand(p)
	help.GroupID = moreGroup
	root.SetHelpCommand(help)
	root.SetCompletionCommandGroupID(moreGroup)
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { writeHelp(cmd, cmd.OutOrStdout(), false) })

	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: fmt.Sprintf("write a starter %s at the repository root", brand.ConfigFile),
		Long: fmt.Sprintf("Write a starter %s at the root of the Git repository you run it in, naming the Compose file found there "+
			"(compose.yaml if none), with a commented-out service and hooks to fill in. It refuses to replace an existing %s. "+
			"Run `%s config validate` afterwards. It reads no %s, so --config is refused.",
			brand.ConfigFile, brand.ConfigFile, commandName, brand.ConfigFile),
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Init(cmd.Context(), env)
		},
	})

	config := &cobra.Command{
		Use:   "config",
		Short: "inspect the project configuration",
		Args:  usageArgs(unknownSubcommand),
		RunE:  bareGroup(p),
	}
	config.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: fmt.Sprintf("parse and validate %s", brand.ConfigFile),
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.ConfigValidate(cmd.Context(), env)
		},
	})
	config.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "print the resolved configuration as JSON",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.ConfigShow(cmd.Context(), env)
		},
	})
	config.AddCommand(&cobra.Command{
		Use:   "schema",
		Short: fmt.Sprintf("print the JSON Schema of %s", brand.ConfigFile),
		Long: fmt.Sprintf("Print the JSON Schema (draft-07) of %s, for editors to complete and check it with. "+
			"It is generated from the same definitions that validate the file, but it cannot state every check: "+
			"`%s config validate` makes the rest. It needs no %s.", brand.ConfigFile, commandName, brand.ConfigFile),
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.ConfigSchema(cmd.Context(), env)
		},
	})
	root.AddCommand(config)

	root.AddCommand(&cobra.Command{
		Use:   "dev",
		Short: "start this worktree's services, then follow their logs until Ctrl-C",
		Long: fmt.Sprintf("Do everything `%s up` does (register the worktree, start the shared router and any shared services, "+
			"start the services, run its hooks), then follow the logs of the worktree's services until you press Ctrl-C. "+
			"The services keep running afterwards; `%s down` stops them, and the session may stay open, idle, until you press Ctrl-C. "+
			"While a session is attached, `%s worktree remove` and a second `%s dev` refuse, but `up`, `down` and `run` work as usual. "+
			"Ctrl-C, SIGTERM and SIGHUP end the session, stop the log follower and release the lock, with exit status 130, 143 and 129. "+
			"If `up` fails, so does `dev`, the same way, and no logs are followed. "+
			"If the log follower ends on its own, because Docker went away or every container is gone, `dev` says so and exits non-zero.",
			commandName, commandName, commandName, commandName),
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Dev(cmd.Context(), env)
		},
	})

	var upReallocate []string
	up := &cobra.Command{
		Use:   "up [services...]",
		Short: "register this worktree, start its services, and run its hooks",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Up(cmd.Context(), env, commands.UpOptions{Services: args, ReallocatePorts: upReallocate})
		},
	}
	up.Flags().StringArrayVar(&upReallocate, "reallocate-port", nil,
		"move a routed service to a new loopback port (repeatable)")
	up.ValidArgsFunction = serviceCompletionFunc(p)
	root.AddCommand(up)

	root.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "stop this worktree's services; data is kept",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Down(cmd.Context(), env)
		},
	})

	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "show service status for this worktree",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Status(cmd.Context(), env, commands.OutputOptions{JSON: statusJSON})
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "machine-readable output")
	root.AddCommand(statusCmd)

	var urlsJSON bool
	urlsCmd := &cobra.Command{
		Use:   "urls",
		Short: "print this worktree's routed URLs",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Urls(cmd.Context(), env, commands.OutputOptions{JSON: urlsJSON})
		},
	}
	urlsCmd.Flags().BoolVar(&urlsJSON, "json", false, "machine-readable output")
	root.AddCommand(urlsCmd)

	run := &cobra.Command{
		Use:   "run [name] [-- args...]",
		Short: fmt.Sprintf("run one of the project's named commands from %s [commands]", brand.ConfigFile),
		Long: fmt.Sprintf("Run one of the commands %s declares under [commands], in this registered worktree, "+
			"with the worktree's TREERIG_* environment and TREERIG_COMMAND naming it. Everything after -- is passed to the command. "+
			"It runs on the terminal, so it can prompt and answer Ctrl-C itself, and its output is this command's own; "+
			"its exit code becomes treerig run's. Unless it sets lock = false, it holds the worktree's lock while it runs. "+
			"docs/hooks.md has the details.", brand.ConfigFile),
		Args: runArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			name, rest := splitRunArgs(cmd, args)
			return commands.Run(cmd.Context(), env, commands.RunOptions{Name: name, Args: rest})
		},
	}
	run.SetFlagErrorFunc(runFlagError)
	run.ValidArgsFunction = commandCompletionFunc(p)
	root.AddCommand(run)

	backup := &cobra.Command{
		Use:   "backup [dir]",
		Short: "run the backup hook, writing a bundle into a directory",
		Long: fmt.Sprintf("Run the backup hook %s declares under [hooks], in this worktree, which must be registered and have a running service, "+
			"with the worktree's TREERIG_* environment and the bundle directory as its first argument. "+
			"The directory is created, or must be empty if it exists; without one the bundle goes to a timestamped "+
			"directory under the worktree's home, which worktree remove deletes along with it. "+
			"The path is reported on stderr when the hook succeeds. "+
			"The hook's exit code becomes treerig backup's, and the worktree's lock is held while it runs. "+
			"docs/hooks.md has the details.", brand.ConfigFile),
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Backup(cmd.Context(), env, commands.BackupOptions{Dir: arg0(args)})
		},
	}
	backup.ValidArgsFunction = directoryCompletionFunc
	root.AddCommand(backup)

	restore := &cobra.Command{
		Use:   "restore <dir>",
		Short: "run the restore hook on a bundle directory",
		Long: fmt.Sprintf("Run the restore hook %s declares under [hooks], in this worktree, which must be registered and have a running service, "+
			"with the worktree's TREERIG_* environment and a bundle directory as its first argument. "+
			"The directory must already exist and is never created; a bundle that a backup did not finish is refused. "+
			"The hook's exit code becomes treerig restore's, and the worktree's lock is held while it runs. "+
			"docs/hooks.md has the details.", brand.ConfigFile),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return clierr.Newf("Restore needs a bundle directory: %s restore <dir>.", commandName)
			}
			return maxArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Restore(cmd.Context(), env, commands.RestoreOptions{Dir: arg0(args)})
		},
	}
	restore.ValidArgsFunction = directoryCompletionFunc
	root.AddCommand(restore)

	var indexJSON bool
	indexCmd := &cobra.Command{
		Use:   "index",
		Short: "refresh the machine-wide checkout index and print it",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RefreshIndex(cmd.Context(), env, commands.OutputOptions{JSON: indexJSON})
		},
	}
	indexCmd.Flags().BoolVar(&indexJSON, "json", false, "machine-readable output")
	root.AddCommand(indexCmd)

	var doctorJSON bool
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "check prerequisites, the registry, the router, and this checkout's verify hook",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			code, err := commands.Doctor(cmd.Context(), env, commands.OutputOptions{JSON: doctorJSON, Version: version})
			if err != nil {
				return err
			}
			if code != 0 {
				p.exit(code)
			}
			return nil
		},
	}
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "machine-readable output")
	root.AddCommand(doctorCmd)

	router := &cobra.Command{
		Use:   "router",
		Short: "manage the shared local router",
		Args:  usageArgs(unknownSubcommand),
		RunE:  bareGroup(p),
	}
	router.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "start the shared router and wait until it is healthy",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RouterUp(cmd.Context(), env)
		},
	})
	router.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "report whether the shared router is running as expected",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			failed, err := commands.RouterStatus(cmd.Context(), env)
			if err != nil {
				return err
			}
			if failed {
				p.exit(1)
			}
			return nil
		},
	})
	router.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "stop the shared router; worktree services keep running but are no longer routed",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RouterDown(cmd.Context(), env)
		},
	})
	root.AddCommand(router)

	routeGroup := &cobra.Command{
		Use:   "route",
		Short: "route processes running on the host through this worktree's hostname",
		Long: "A host route sends a hostname of this worktree to a process running on the host (a dev server, an API run from the IDE), " +
			"through the same router as its services. The process must listen on 127.0.0.1. `" + commandName + " up` and `" + commandName +
			" down` leave host routes alone; `" + commandName + " worktree remove` removes them. The route commands need a registered worktree: run `" + commandName + " up` first.",
		Args: usageArgs(unknownSubcommand),
		RunE: bareGroup(p),
	}
	var routePort int
	var routeSubdomain, routePath string
	routeAdd := &cobra.Command{
		Use:   "add <name> --port <port>",
		Short: "route this worktree's hostname, or a subdomain or path of it, to a process on the host",
		Long: "Route a hostname of this worktree to the process listening on 127.0.0.1:<port>. " +
			"Like a [service.<name>] table, the route takes a --subdomain and a --path, and gets the hostname a service with that name and those fields would get. " +
			"The name must not be a service's or another host route's, and the route must not duplicate one; adding a route identical to a recorded one writes its route file again. " +
			"Needs a registered worktree: run `" + commandName + " up` first.",
		Args: exactArgs(1, "the route's name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("port") {
				return clierr.New("--port is required: the port the process on the host listens on.")
			}
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			options := commands.RouteAddOptions{Name: args[0], Port: routePort, Subdomain: routeSubdomain}
			if cmd.Flags().Changed("path") {
				options.Path = &routePath
			}
			return commands.RouteAdd(cmd.Context(), env, options)
		},
	}
	routeAdd.Flags().IntVar(&routePort, "port", 0, "the port the process on the host listens on, at 127.0.0.1 (required)")
	routeAdd.Flags().StringVar(&routeSubdomain, "subdomain", "", "route <subdomain>.<this worktree's host> instead of the host itself")
	routeAdd.Flags().StringVar(&routePath, "path", "", "route only this URL path prefix, which must start with /")
	routeGroup.AddCommand(routeAdd)
	routeRm := &cobra.Command{
		Use:   "rm <name>",
		Short: "remove a host route",
		Long: "Remove a host route of this worktree: its route file, then its entry in the registration. " +
			"Needs a registered worktree: run `" + commandName + " up` first. `" + commandName + " route ls` lists the names.",
		Args: exactArgs(1, "the route's name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RouteRm(cmd.Context(), env, args[0])
		},
	}
	routeRm.ValidArgsFunction = routeNameCompletionFunc(p)
	routeGroup.AddCommand(routeRm)
	var routeLsJSON bool
	routeLs := &cobra.Command{
		Use:   "ls",
		Short: "list this worktree's host routes",
		Long: "List this worktree's host routes, one per line: the name, the URL it answers on, and the 127.0.0.1 port it targets. " +
			"Needs a registered worktree: run `" + commandName + " up` first. With --json, an array of {name, url, port}, plus subdomain and path when the route has them.",
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RouteLs(cmd.Context(), env, commands.OutputOptions{JSON: routeLsJSON})
		},
	}
	routeLs.Flags().BoolVar(&routeLsJSON, "json", false, "machine-readable output")
	routeGroup.AddCommand(routeLs)
	root.AddCommand(routeGroup)

	sharedGroup := &cobra.Command{
		Use:   "shared",
		Short: "manage the repository's shared services",
		Long: fmt.Sprintf("Shared services run once per repository, in a Compose project of their own, and are connected to each worktree whose services use them: "+
			"declare them under [shared] in %s. `%s up` starts the ones a worktree uses; these commands start, replace, stop and inspect them yourself.", brand.ConfigFile, commandName),
		Args: usageArgs(unknownSubcommand),
		RunE: bareGroup(p),
	}
	var sharedRecreate bool
	var sharedReallocate []string
	sharedUp := &cobra.Command{
		Use:   "up [name...]",
		Short: "start shared services, and reconnect every worktree that uses them (--recreate <name> replaces a running one)",
		Long: "Start the named shared services, or all of them, and reconnect every worktree that uses them. " +
			"A service that is already running is left alone: one whose definition differs from this worktree's is refused unless you pass --recreate with its name, " +
			"which replaces that service with this worktree's definition and keeps its data. This is also how a worktree left unconnected, by a busy lock or a daemon restart, is repaired.",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.SharedUp(cmd.Context(), env, commands.SharedUpOptions{Names: args, Recreate: sharedRecreate, ReallocatePorts: sharedReallocate})
		},
	}
	sharedUp.Flags().BoolVar(&sharedRecreate, "recreate", false, "replace running services with this worktree's definition, keeping their data")
	sharedUp.Flags().StringArrayVar(&sharedReallocate, "reallocate-port", nil,
		"move a container port to a new loopback port, as <name>:<container port>; refused while the service runs (repeatable)")
	sharedUp.ValidArgsFunction = sharedCompletionFunc(p)
	sharedGroup.AddCommand(sharedUp)

	var sharedVolumes, sharedYes bool
	var sharedNamespace string
	sharedDown := &cobra.Command{
		Use:   "down [name...]",
		Short: "stop shared services; their data is kept unless you pass --volumes",
		Long: "Stop and remove the named shared services' containers, or all of them, found by their labels from any checkout of the repository: " +
			"no " + brand.ConfigFile + " is needed. Their data and pinned ports are kept; --volumes deletes the data and releases the ports, and asks you to confirm, " +
			"as `worktree remove` does. It does not ask whether worktrees are using them.",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.SharedDown(cmd.Context(), env, commands.SharedDownOptions{Names: args, Volumes: sharedVolumes, Yes: sharedYes, LabelNamespace: sharedNamespace})
		},
	}
	sharedDown.Flags().BoolVar(&sharedVolumes, "volumes", false, "also delete the data volumes and release the pinned ports")
	sharedDown.Flags().BoolVar(&sharedYes, "yes", false, "skip the confirmation phrase")
	sharedDown.Flags().StringVar(&sharedNamespace, "label-namespace", "", "supply the Docker label namespace when there is no usable "+brand.ConfigFile+" to say it")
	sharedDown.ValidArgsFunction = sharedCompletionFunc(p)
	sharedGroup.AddCommand(sharedDown)

	var sharedStatusJSON bool
	sharedStatus := &cobra.Command{
		Use:   "status",
		Short: "show the shared services: state, addresses, definition and connection",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.SharedStatus(cmd.Context(), env, commands.OutputOptions{JSON: sharedStatusJSON})
		},
	}
	sharedStatus.Flags().BoolVar(&sharedStatusJSON, "json", false, "machine-readable output")
	sharedGroup.AddCommand(sharedStatus)
	root.AddCommand(sharedGroup)

	worktree := &cobra.Command{
		Use:   "worktree",
		Short: "manage registered worktrees",
		Args:  usageArgs(unknownSubcommand),
		RunE:  bareGroup(p),
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "list this repository's worktrees, or every registered worktree outside a repository",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			all, _ := cmd.Flags().GetBool("all")
			jsonOut, _ := cmd.Flags().GetBool("json")
			return commands.List(cmd.Context(), env, commands.ListOptions{All: all, JSON: jsonOut})
		},
	}
	list.Flags().Bool("all", false, "every repository and domain on this machine")
	list.Flags().Bool("json", false, "machine-readable output")
	worktree.AddCommand(list)

	remove := &cobra.Command{
		Use:   "remove [worktree]",
		Short: "remove a worktree's environment: containers, volumes, networks, images, home, and registration",
		Long:  "Remove a worktree's environment: containers, volumes, networks, images, home, and registration.\n\n" + worktreeNameHelp,
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			domain, _ := cmd.Flags().GetString("domain")
			labelNamespace, _ := cmd.Flags().GetString("label-namespace")
			yes, _ := cmd.Flags().GetBool("yes")
			devTeardown, _ := cmd.Flags().GetBool("dev-teardown")
			return commands.Remove(cmd.Context(), env, commands.RemoveOptions{
				Identifier:     arg0(args),
				Domain:         domain,
				LabelNamespace: labelNamespace,
				Yes:            yes,
				DevTeardown:    devTeardown,
				Version:        version,
			})
		},
	}
	remove.Flags().String("domain", "", "narrow a slug registered under more than one domain")
	remove.Flags().String("label-namespace", "", "supply the Docker label namespace when nothing on disk records it (a worktree of another repository, or run without a treerig.toml)")
	remove.Flags().Bool("yes", false, "skip the confirmation phrase")
	remove.Flags().Bool("dev-teardown", false, "remove with this build's own code, not a pinned teardown binary")
	// Still accepted, but not advertised: it is a developer's escape hatch,
	// and the refusal on a development build names it.
	_ = remove.Flags().MarkHidden("dev-teardown")
	remove.ValidArgsFunction = identifierCompletionFunc(p, false)
	_ = remove.RegisterFlagCompletionFunc("domain", domainCompletionFunc(p))
	worktree.AddCommand(remove)

	rename := &cobra.Command{
		Use:   "rename <new-slug>",
		Short: "change this worktree's slug, and with it its hostname",
		Long: "Rename the worktree you run it from: change the slug it is registered under, so it routes as <new-slug>.<domain>. " +
			"A slug is a lowercase hostname label: letters, digits and inner hyphens, at most 63 characters. " +
			"The registration and index entry move to the new slug and any leftover route files are removed; `up` writes them at the new hostname. " +
			"The data directory, volumes and pinned ports are keyed by the worktree's ID and stay as they are. " +
			fmt.Sprintf("It refuses a slug another worktree holds in the same domain, the reserved slug \"shared\", and a worktree that still has containers, running or stopped: run `%s down` first.", commandName),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return clierr.Newf("%q needs the new slug as its one argument.", cmd.CommandPath())
			}
			return maxArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Rename(cmd.Context(), env, commands.RenameOptions{Slug: args[0]})
		},
	}
	rename.ValidArgsFunction = func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return noCompletions()
	}
	worktree.AddCommand(rename)

	unlock := &cobra.Command{
		Use:   "unlock [worktree]",
		Long:  "Clear a dead holder's lock on a worktree, its repository's shared services, the registry, or the router, once the holder, and anything it left running, is checked gone.\n\n" + worktreeNameHelp,
		Short: "clear a dead holder's lock on a worktree, its repository's shared services, the registry, or the router, once the holder, and anything it left running, is checked gone",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			domain, _ := cmd.Flags().GetString("domain")
			registry, _ := cmd.Flags().GetBool("registry")
			routerFlag, _ := cmd.Flags().GetBool("router")
			sharedFlag, _ := cmd.Flags().GetBool("shared")
			allowUnverifiable, _ := cmd.Flags().GetBool("holder-unverifiable")
			return commands.Unlock(cmd.Context(), env, commands.UnlockOptions{
				Identifier:        arg0(args),
				Domain:            domain,
				Registry:          registry,
				Router:            routerFlag,
				Shared:            sharedFlag,
				AllowUnverifiable: allowUnverifiable,
			})
		},
	}
	unlock.Flags().String("domain", "", "narrow a slug registered under more than one domain")
	unlock.Flags().Bool("registry", false, "clear the machine-wide worktree registry lock instead of a worktree's")
	unlock.Flags().Bool("router", false, "clear the shared router's lock instead of a worktree's")
	unlock.Flags().Bool("shared", false, "clear the shared-services lock of the worktree's repository instead of the worktree's own locks")
	unlock.Flags().Bool("holder-unverifiable", false, "accept a holder this machine cannot check (another machine or container, or no record), once you have made sure it is gone")
	unlock.ValidArgsFunction = identifierCompletionFunc(p, true)
	_ = unlock.RegisterFlagCompletionFunc("domain", domainCompletionFunc(p))
	worktree.AddCommand(unlock)

	root.AddCommand(worktree)

	teardownGroup := &cobra.Command{
		Use:   "teardown",
		Short: "pin and check the trusted treerig build for worktree removal",
		Long: "The pinned teardown binary is the build of treerig that `worktree remove` runs in place of whichever build you invoke, so that code from a branch does not run during a removal. " +
			"These commands install the pin and check it. " +
			"A development build's `worktree remove` refuses without a pin. A release build's removes with itself. Either runs the pinned binary when one exists. " +
			"`worktree remove --dev-teardown` removes with the build you invoke instead.",
		Args: usageArgs(unknownSubcommand),
		RunE: bareGroup(p),
	}
	var pinFile, pinSHA256, pinRelease string
	var pinLatest, pinYes bool
	pin := &cobra.Command{
		Use:   "pin (--release X.Y.Z | --latest | --file PATH)",
		Short: "pin a treerig release, or a treerig binary you have verified, as the teardown binary",
		Long: "Install a treerig binary as the teardown pin, by downloading a release (`pin --release X.Y.Z` or `pin --latest`) or from a file (`pin --file PATH --sha256 HEX --release LABEL`). " +
			"With --release X.Y.Z, or --latest for the newest release that is not a pre-release, treerig downloads that release from GitHub, " +
			"has cosign (3.0.0 or newer, found on your PATH) verify the release's checksums.txt against the release workflow's signature, " +
			"checks the archive for this platform against it, and pins the `treerig` inside. Nothing is pinned until you approve the plan. " +
			"With --file PATH, treerig pins a local binary you have verified yourself: you give its sha256, and this approves exactly the bytes you hold. " +
			"Take the digest of the binary itself (for a download, extract `treerig` from the archive you verified, then run `sha256sum treerig`); " +
			"it is a meaningful check only when it comes from a second trusted source, such as your own build. " +
			"The file is read once, those bytes are hashed and compared with it, and the same bytes are stored in the control home. " +
			"With --file, no signature is checked and cosign is not run; --release is then a label for the pin. " +
			"Nothing is run from the pinned binary. There is one pin per control home, and pinning again replaces it, " +
			"or adds this platform to it when it is the same release. " +
			"The command shows what it will pin and asks you to type a phrase, unless you pass --yes.",
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.TeardownPin(cmd.Context(), env, commands.TeardownPinOptions{File: pinFile, SHA256: pinSHA256, Release: pinRelease, Latest: pinLatest, Yes: pinYes})
		},
	}
	pin.Flags().StringVar(&pinFile, "file", "", "pin this treerig binary instead of downloading a release; needs --sha256 and --release")
	pin.Flags().StringVar(&pinSHA256, "sha256", "", "with --file: the sha256 of that file itself, from a source you trust")
	pin.Flags().StringVar(&pinRelease, "release", "", "the release to download and verify, such as 1.2.3 or 0.1.0-rc.1 (no leading v); with --file, a label for the pin instead")
	pin.Flags().BoolVar(&pinLatest, "latest", false, "download the newest release that is not a pre-release")
	pin.Flags().BoolVar(&pinYes, "yes", false, "skip the confirmation phrase")
	// The one flag that names a file in this tree besides --config; addCompletion
	// gives every other string flag no completions at all.
	_ = pin.RegisterFlagCompletionFunc("file", func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveDefault
	})
	teardownGroup.AddCommand(pin)
	var teardownStatusJSON bool
	teardownStatus := &cobra.Command{
		Use:   "status",
		Short: "check the teardown pin and its stored binary",
		Long: "Check the teardown pin on this machine: that it can be read, and that the stored binary for this platform exists and still has the sha256 the pin records. " +
			"Exit 0 means removal would run: the pin is intact, or there is none and this is a release build, which needs none. " +
			"A development build with no pin, or any other problem, is reported on stderr with exit 1. " +
			"The text report ends with a line saying what `worktree remove` would do, which also takes into account where the control home is and who can write it. " +
			"With --json, one document on stdout whose \"removal\" is \"pin\", \"this-build\" or \"refuse\"; with no pin, {\"pinned\": false, \"removal\": …}; empty stdout with exit 1 means the pin could not be read.",
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.TeardownStatus(cmd.Context(), env, commands.TeardownStatusOptions{JSON: teardownStatusJSON, Version: version})
		},
	}
	teardownStatus.Flags().BoolVar(&teardownStatusJSON, "json", false, "machine-readable output")
	teardownGroup.AddCommand(teardownStatus)
	root.AddCommand(teardownGroup)

	root.SetOut(stdout)
	root.SetErr(stderr)
	declareGroupsAndConfig(root)
	// Must run after every RegisterFlagCompletionFunc above it (identifier
	// and --domain completion on remove and unlock): addCompletion's own
	// loop registers cobra.NoFileCompletions on every string flag that has
	// nothing registered yet, and cobra refuses — silently, since the error
	// is discarded — a second registration for one that already does, so a
	// call here ahead of those would leave --domain's own completion
	// clobbered by the blanket default instead of the other way around.
	addCompletion(root, p)

	return root
}
