package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/commands"
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

// env resolves cmd's own --config (root's persistent flag, inherited by
// every command) into a commands.Env for cmd's action.
func (p *program) env(cmd *cobra.Command) (*commands.Env, error) {
	configPath, _ := cmd.Flags().GetString("config")
	return p.buildEnv(configPath)
}

// exit records the exit code the process should end with when the current
// action returns nil: doctor's own diagnostic code, or router status's
// already-printed failure.
func (p *program) exit(code int) { *p.exitCode = code }

// usageArgs adapts a cobra.PositionalArgs validator so a failure it reports
// is treated the way every other parse-time mistake in this tree is: a
// clierr.Error, which the top-level report() prints as "[name] ERROR: ...",
// exit 1, with no BERTH_DEBUG hint — rather than the crash report() takes
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

// flagError is the whole tree's flag-parse error handler: pflag's own
// message as a clierr.Error, plus a "Did you mean this?" list for an unknown
// long flag, in the same shape unknownSubcommand gives an unknown command
// (pflag, unlike cobra, suggests nothing itself).
func flagError(cmd *cobra.Command, err error) error {
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
		writeHelp(cmd, cmd.ErrOrStderr())
		p.exit(1)
		return nil
	}
}

// newHelpCommand is this tree's own "help [command]" leaf, replacing
// cobra's default one: cobra's own help command calls CheckErr, which can
// exit the process directly on a lookup failure, bypassing this package's
// own exit-code and signal handling entirely. Found, it shows a command's
// own help, to stdout, exit 0 — walking the full path given, not just its
// first word. Not found, it falls back to the whole program's own help, to
// stderr, exit 1, the same as any other unresolvable name in this tree.
func newHelpCommand(p *program) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "display help for command",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			target, _, err := root.Find(args)
			// Find never fails to match a command on this tree — every
			// group's own Args validator (unknownSubcommand) is non-nil, so
			// Find leaves that check to ValidateArgs instead (see
			// unknownSubcommand's own doc comment) — so a name that matched
			// nothing is target == root with args left unconsumed, not a
			// non-nil err. Bare "help" also resolves to root, but with
			// nothing left over: that is success, not failure.
			resolved := err == nil && (target != root || len(args) == 0)
			if !resolved {
				writeHelp(root, root.ErrOrStderr())
				p.exit(1)
				return nil
			}
			// Executing a command the normal way is what registers its own
			// -h/--help flag (InitDefaultHelpFlag, called from execute());
			// bypassing execute() to jump straight to Help(), as this does,
			// has to register it by hand first, the same way cobra's own
			// default help command does.
			target.InitDefaultHelpFlag()
			writeHelp(target, root.OutOrStdout())
			return nil
		},
	}
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
	// --config names a file; no positional argument and no other flag value
	// in this tree does. Flag completions live in a cobra-wide map, locked
	// for concurrent use, that gains an entry for each flag of each tree.
	_ = root.MarkPersistentFlagFilename("config", "toml")
	visitCommands(root, func(cmd *cobra.Command) {
		if cmd.ValidArgsFunction == nil && len(cmd.ValidArgs) == 0 {
			cmd.ValidArgsFunction = cobra.NoFileCompletions
		}
		cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "string" {
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

// arg0 is args[0], or "" if there isn't one: every leaf in this tree that
// takes an optional identifier takes at most one.
func arg0(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

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
	root.PersistentFlags().String("config", "", fmt.Sprintf("path to %s (default: nearest above cwd)", brand.ConfigFile))
	root.PersistentFlags().BoolP("version", "V", false, "output the version number")
	root.SetFlagErrorFunc(flagError)
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if v, _ := cmd.Flags().GetBool("version"); v {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return errVersionRequested
		}
		return nil
	}
	root.RunE = bareGroup(p)
	root.SetHelpCommand(newHelpCommand(p))
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { writeHelp(cmd, cmd.OutOrStdout()) })

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
	root.AddCommand(config)

	// TODO(#5): once `dev` starts things, it needs the same
	// config.CheckStart call `up` and `config validate` make before it
	// touches anything, refusing a control character in a [service.index]
	// label or purpose the same way.
	root.AddCommand(&cobra.Command{
		Use:   "dev",
		Short: "start this worktree's environment and follow its logs (not yet implemented)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return clierr.WithCode(fmt.Sprintf("'%s dev' is not implemented yet; see the Roadmap in README.md.", commandName), 2)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "up [services...]",
		Short: "register this worktree, start its services, and run its hooks",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Up(cmd.Context(), env, commands.UpOptions{Services: args})
		},
	})

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

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "show service status for this worktree",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Status(cmd.Context(), env)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "urls",
		Short: "print this worktree's routed URLs",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.Urls(cmd.Context(), env)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "index",
		Short: "refresh the machine-wide checkout index and print it",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			return commands.RefreshIndex(cmd.Context(), env)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "check prerequisites, the registry, the router, and this checkout's verify hook",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			code, err := commands.Doctor(cmd.Context(), env)
			if err != nil {
				return err
			}
			if code != 0 {
				p.exit(code)
			}
			return nil
		},
	})

	router := &cobra.Command{
		Use:   "router",
		Short: "manage the shared local router",
		Args:  usageArgs(unknownSubcommand),
		RunE:  bareGroup(p),
	}
	router.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "start the shared router and verify its hardened configuration",
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

	worktree := &cobra.Command{
		Use:   "worktree",
		Short: "manage registered worktrees",
		Args:  usageArgs(unknownSubcommand),
		RunE:  bareGroup(p),
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "list registered worktrees",
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
		Use:   "remove [identifier]",
		Short: "remove a worktree's environment: containers, volumes, networks, images, home, and registration",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			domain, _ := cmd.Flags().GetString("domain")
			labelNamespace, _ := cmd.Flags().GetString("label-namespace")
			yes, _ := cmd.Flags().GetBool("yes")
			return commands.Remove(cmd.Context(), env, commands.RemoveOptions{
				Identifier:     arg0(args),
				Domain:         domain,
				LabelNamespace: labelNamespace,
				Yes:            yes,
			})
		},
	}
	remove.Flags().String("domain", "", "narrow a slug registered under more than one domain")
	remove.Flags().String("label-namespace", "", "supply the Docker label namespace when nothing on disk records it (a worktree of another repository)")
	remove.Flags().Bool("yes", false, "skip the confirmation phrase")
	worktree.AddCommand(remove)

	unlock := &cobra.Command{
		Use:   "unlock [identifier]",
		Short: "clear a dead holder's lock on a worktree, the registry, or the router, once the holder, and anything it left running, is checked gone",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := p.env(cmd)
			if err != nil {
				return err
			}
			domain, _ := cmd.Flags().GetString("domain")
			registry, _ := cmd.Flags().GetBool("registry")
			routerFlag, _ := cmd.Flags().GetBool("router")
			allowUnverifiable, _ := cmd.Flags().GetBool("holder-unverifiable")
			return commands.Unlock(cmd.Context(), env, commands.UnlockOptions{
				Identifier:        arg0(args),
				Domain:            domain,
				Registry:          registry,
				Router:            routerFlag,
				AllowUnverifiable: allowUnverifiable,
			})
		},
	}
	unlock.Flags().String("domain", "", "narrow a slug registered under more than one domain")
	unlock.Flags().Bool("registry", false, "clear the machine-wide worktree registry lock instead of a worktree's")
	unlock.Flags().Bool("router", false, "clear the shared router's lock instead of a worktree's")
	unlock.Flags().Bool("holder-unverifiable", false, "accept a holder this machine cannot check (another machine or container, or no record), once you have made sure it is gone")
	worktree.AddCommand(unlock)

	root.AddCommand(worktree)

	root.SetOut(stdout)
	root.SetErr(stderr)
	addCompletion(root, p)

	return root
}
