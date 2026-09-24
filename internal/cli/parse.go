package cli

// The dispatch and option-scanning algorithm below ports command.js's
// _parseCommand/parseOptions and their error paths, restricted to what this
// tree exercises: no executable subcommands, hooks, positional-options mode,
// mandatory or conflicting options, option environment variables, or
// variadic *options* (only the "up" command's [services...] *argument* is
// variadic). See model.go's package comment for the licensing note that
// covers this file too.
//
// Commander error()s and help by writing straight to the configured stdout
// or stderr — unprefixed, no "[name]" — and exits. Nothing here goes through
// internal/logger: that is reserved for what a command itself reports once
// running (see runContext and the top-level report() in cli.go).

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sofired/berth/internal/commands"
)

// exitSignal is a parser-level outcome that has already written its output:
// help shown, version printed, or a commander-style parse error. dispatch
// returns it all the way up to Main without going through clierr/report().
type exitSignal struct{ code int }

// parseState carries what every node in one call to dispatch needs: where
// parser-level output goes, and how to build the commands.Env a leaf action
// runs with.
type parseState struct {
	ctx                context.Context
	stdout, stderr     io.Writer
	outWidth, errWidth int
	root               *node
	buildEnv           func(configPath string) (*commands.Env, error)
}

func (ps *parseState) helpWidth(isError bool) int {
	if isError {
		return ps.errWidth
	}
	return ps.outWidth
}

func (ps *parseState) writerFor(isError bool) io.Writer {
	if isError {
		return ps.stderr
	}
	return ps.stdout
}

// showHelp writes n's help to stdout (isError false, exit 0) or stderr
// (isError true, exit 1) — command.js's help({error}). formatHelp's own
// output already ends in exactly one newline (its last section always ends
// with a blank line, courtesy of formatItemList — every node has at least
// the implicit help option, so the Options: section is never empty), the
// way commander's own write(helpInformation) adds none of its own.
func (ps *parseState) showHelp(n *node, isError bool) *exitSignal {
	text := formatHelp(n, ps.helpWidth(isError))
	_, _ = io.WriteString(ps.writerFor(isError), text)
	code := 0
	if isError {
		code = 1
	}
	return &exitSignal{code: code}
}

// errorExit is command.js's error(): a single unprefixed line to stderr,
// exit 1. Every parse-time failure in this tree uses the default exit code;
// none opts into a different one the way version does with 0.
func (ps *parseState) errorExit(message string) *exitSignal {
	_, _ = io.WriteString(ps.stderr, message+"\n")
	return &exitSignal{code: 1}
}

func (ps *parseState) missingArgument(name string) *exitSignal {
	return ps.errorExit(fmt.Sprintf("error: missing required argument '%s'", name))
}

func (ps *parseState) optionMissingArgument(o *option) *exitSignal {
	return ps.errorExit(fmt.Sprintf("error: option '%s' argument missing", o.flags))
}

// excessArguments is command.js's _excessArguments(): "too many arguments"
// for n, or with no parent (the root has none in this tree) "too many
// arguments" alone.
func (ps *parseState) excessArguments(n *node, received []string) *exitSignal {
	expected := len(n.args)
	plural := "s"
	if expected == 1 {
		plural = ""
	}
	forSubcommand := ""
	if n.parent != nil {
		forSubcommand = fmt.Sprintf(" for '%s'", n.name)
	}
	return ps.errorExit(fmt.Sprintf(
		"error: too many arguments%s. Expected %d argument%s but got %d: %s.",
		forSubcommand, expected, plural, len(received), strings.Join(received, ", "),
	))
}

// unknownOption is command.js's unknownOption(): the flag and, for a long
// flag, a suggestion drawn from this node's own options and every ancestor's
// (commander's walk stops only at a positional-options boundary, which this
// tree never sets).
func (ps *parseState) unknownOption(n *node, flag string) *exitSignal {
	suggestion := ""
	if strings.HasPrefix(flag, "--") {
		var candidates []string
		for cur := n; cur != nil; cur = cur.parent {
			for _, o := range cur.visibleOptions() {
				if o.long != "" {
					candidates = append(candidates, o.long)
				}
			}
		}
		suggestion = suggestSimilar(flag, candidates)
	}
	return ps.errorExit(fmt.Sprintf("error: unknown option '%s'%s", flag, suggestion))
}

// unknownCommand is command.js's unknownCommand(): candidates are this
// node's own visible subcommands (including its implicit help
// pseudo-command), not its ancestors'.
func (ps *parseState) unknownCommand(n *node, name string) *exitSignal {
	var candidates []string
	for _, c := range n.visibleCommands() {
		candidates = append(candidates, c.name)
	}
	suggestion := suggestSimilar(name, candidates)
	return ps.errorExit(fmt.Sprintf("error: unknown command '%s'%s", name, suggestion))
}

// printVersion is command.js's inline 'option:version' listener: write the
// version and exit 0, short-circuiting the rest of the scan.
func (ps *parseState) printVersion(n *node) *exitSignal {
	_, _ = io.WriteString(ps.stdout, n.version+"\n")
	return &exitSignal{code: 0}
}

// maybeOption is parseOptions' function of the same name.
func maybeOption(arg string) bool { return len(arg) > 1 && arg[0] == '-' }

// parseOptions is command.js's parseOptions(), scanning args for this node's
// own registered options (never helpOption — see model.go) and splitting
// what is left into operands and unknown. args => operands, unknown mirrors
// the upstream doc comment's examples exactly.
func (n *node) parseOptions(ps *parseState, args []string) (operands, unknown []string, exit *exitSignal) {
	dest := &operands
	var activeGroup string
	i := 0
	for i < len(args) || activeGroup != "" {
		var arg string
		if activeGroup != "" {
			arg = activeGroup
			activeGroup = ""
		} else {
			arg = args[i]
			i++
		}

		if arg == "--" {
			if dest == &unknown {
				*dest = append(*dest, arg)
			}
			*dest = append(*dest, args[i:]...)
			break
		}

		if maybeOption(arg) {
			if opt := n.findOption(arg); opt != nil {
				if opt.takesValue {
					if i >= len(args) {
						return nil, nil, ps.optionMissingArgument(opt)
					}
					value := args[i]
					i++
					n.setValue(opt, value)
				} else {
					n.setFlag(opt)
					if opt.isVersion {
						return nil, nil, ps.printVersion(n)
					}
				}
				continue
			}
		}

		// Combo short flags, e.g. -cVALUE: eat the first recognised one and
		// keep processing what follows as a new argument.
		if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
			if opt := n.findOption("-" + string(arg[1])); opt != nil {
				if opt.takesValue {
					n.setValue(opt, arg[2:])
				} else {
					n.setFlag(opt)
					if opt.isVersion {
						return nil, nil, ps.printVersion(n)
					}
					activeGroup = "-" + arg[2:]
				}
				continue
			}
		}

		// Known long flag with an inline value, e.g. --config=path.
		if strings.HasPrefix(arg, "--") {
			if eq := strings.IndexByte(arg, '='); eq > 0 {
				if opt := n.findOption(arg[:eq]); opt != nil && opt.takesValue {
					n.setValue(opt, arg[eq+1:])
					continue
				}
			}
		}

		// Not recognised by this node. An unknown option (and everything
		// after it) is reclassified so a subcommand gets a chance to parse it.
		if dest == &operands && maybeOption(arg) {
			dest = &unknown
		}
		*dest = append(*dest, arg)
	}
	return operands, unknown, nil
}

// checkNumberOfArguments is command.js's _checkNumberOfArguments(), folded
// into processArguments below since nothing else needs the check on its own.

// processArguments validates operand count against n.args and returns the
// positional values an action receives verbatim (see runContext.positional):
// this tree defines no argument-level parser or default value, so no other
// processing applies.
func (ps *parseState) processArguments(n *node, operands []string) ([]string, *exitSignal) {
	for i, a := range n.args {
		if a.required && i >= len(operands) {
			return nil, ps.missingArgument(a.name)
		}
	}
	variadic := len(n.args) > 0 && n.args[len(n.args)-1].variadic
	if !variadic && len(operands) > len(n.args) {
		return nil, ps.excessArguments(n, operands)
	}
	return operands, nil
}

// outputHelpIfRequested is command.js's function of the same name: an exact
// "-h" or "--help" among the unrecognised tokens shows this node's help and
// exits 0, whatever else was also unrecognised.
func (ps *parseState) outputHelpIfRequested(n *node, unknown []string) *exitSignal {
	for _, arg := range unknown {
		if arg == helpOption.short || arg == helpOption.long {
			return ps.showHelp(n, false)
		}
	}
	return nil
}

// dispatchHelp is command.js's _dispatchHelpCommand(): only the first word
// after "help" ever matters — "help worktree remove" and "help worktree"
// both show worktree's own help, because commander's default help command
// takes a single, non-variadic argument.
func (ps *parseState) dispatchHelp(n *node, name string) *exitSignal {
	if name == "" {
		return ps.showHelp(n, false)
	}
	if child := n.findChild(name); child != nil {
		return ps.showHelp(child, false)
	}
	return ps.showHelp(n, true)
}

// at is JavaScript's array indexing past the end: "" rather than a panic.
func at(items []string, i int) string {
	if i < 0 || i >= len(items) {
		return ""
	}
	return items[i]
}

// run is command.js's _parseCommand(operands, unknown), dispatching depth
// first through the tree until it reaches a leaf action, an explicit help
// request, or a parse error. Its shape mirrors the upstream method's
// branches in the same order they are checked there.
func (ps *parseState) run(n *node, operands, unknown []string) (int, error) {
	parsedOperands, parsedUnknown, exit := n.parseOptions(ps, unknown)
	if exit != nil {
		return exit.code, nil
	}
	operands = append(operands, parsedOperands...)
	unknown = parsedUnknown
	args := len(operands) + len(unknown)

	if len(operands) > 0 {
		if child := n.findChild(operands[0]); child != nil {
			return ps.run(child, operands[1:], unknown)
		}
	}
	if n.hasImplicitHelp() && at(operands, 0) == "help" {
		return ps.dispatchHelp(n, at(operands, 1)).code, nil
	}
	if len(n.children) > 0 && args == 0 && n.action == nil {
		return ps.showHelp(n, true).code, nil
	}
	if exit := ps.outputHelpIfRequested(n, unknown); exit != nil {
		return exit.code, nil
	}

	if n.action != nil {
		if len(unknown) > 0 {
			return ps.unknownOption(n, unknown[0]).code, nil
		}
		positional, exit := ps.processArguments(n, operands)
		if exit != nil {
			return exit.code, nil
		}
		env, err := ps.buildEnv(ps.root.get("--config").value)
		if err != nil {
			return 0, err
		}
		rc := &runContext{ctx: ps.ctx, env: env, positional: positional, values: n.values}
		if err := n.action(rc); err != nil {
			return 0, err
		}
		return rc.exitCode, nil
	}

	// A group with nothing matched: a real unknown command name, or stray
	// unrecognised options with no command name at all.
	if len(operands) > 0 {
		return ps.unknownCommand(n, operands[0]).code, nil
	}
	if len(unknown) > 0 {
		return ps.unknownOption(n, unknown[0]).code, nil
	}
	return ps.showHelp(n, true).code, nil
}
