// Package cli is the command line: argument parsing, help rendering and
// error reporting for the berth binary. It reproduces the observable
// behaviour of commander 15 (https://github.com/tj/commander.js), the
// framework the TypeScript build parses with, for exactly the command tree
// src/cli/main.ts defines — not as a general-purpose replacement for it.
//
// commander itself is not used: the spike's conventions call for no CLI
// framework dependency, so the slice of its parsing, help and suggestion
// behaviour this tree exercises is ported by hand. Where this package's
// comments describe commander's own behaviour, they paraphrase
// node_modules/commander/lib/{command,help,option,argument,suggestSimilar}.js
// (MIT licensed, Copyright (c) 2011 TJ Holowaychuk <tj@vision-media.ca>).
package cli

import "strings"

// option is a flag a command accepts, either a boolean switch or one that
// takes a value. flags is kept verbatim — it is what error messages and help
// quote back (e.g. "option '--domain <domain>' argument missing").
type option struct {
	flags       string
	short       string // "-h", or "" if this option has no short form
	long        string // "--help", or "" if this option has no long form
	takesValue  bool   // flags contained a <...> placeholder
	description string
	// isVersion marks the root's -V/--version: matching it prints version and
	// exits immediately, the way commander's own emitted 'option:version'
	// listener does mid-scan.
	isVersion bool
}

// newOption parses a commander-style flags string, e.g. "-V, --version" or
// "--config <path>". Mirrors option.js's splitOptionFlags for the subset of
// syntax src/cli/main.ts uses: short and/or long flag, at most one required
// value placeholder. No optional ([value]) or variadic (<value...>) option
// arguments appear in that tree, so they are not supported here.
func newOption(flags, description string) *option {
	o := &option{flags: flags, description: description}
	for _, field := range strings.FieldsFunc(flags, func(r rune) bool { return r == ' ' || r == ',' }) {
		switch {
		case strings.HasPrefix(field, "--"):
			o.long = field
		case strings.HasPrefix(field, "-"):
			o.short = field
		case strings.HasPrefix(field, "<"):
			o.takesValue = true
		}
	}
	return o
}

// helpOption is shared by every command. Unlike a command's own options, it
// is never part of that command's options slice — commander keeps it out of
// parseOptions' recognised-option scan and only folds it in for display and
// for the dedicated --help/-h check, and the same split is reproduced here
// (see node.outputHelpIfRequested and node.visibleOptions).
var helpOption = &option{flags: "-h, --help", short: "-h", long: "--help", description: "display help for command"}

// argument is a positional a command accepts.
type argument struct {
	name        string
	required    bool
	variadic    bool
	description string
}

// newArgument parses a commander-style argument spec, e.g. "[identifier]" or
// "[services...]". Mirrors argument.js's constructor.
func newArgument(spec, description string) *argument {
	a := &argument{description: description}
	name := spec
	switch {
	case strings.HasPrefix(spec, "<"):
		a.required = true
		name = strings.TrimSuffix(strings.TrimPrefix(spec, "<"), ">")
	case strings.HasPrefix(spec, "["):
		a.required = false
		name = strings.TrimSuffix(strings.TrimPrefix(spec, "["), "]")
	default:
		a.required = true
	}
	if strings.HasSuffix(name, "...") {
		a.variadic = true
		name = strings.TrimSuffix(name, "...")
	}
	a.name = name
	return a
}

// humanReadableArgName is argument.js's function of the same name: <name> for
// a required argument, [name] for an optional one, with "..." kept for a
// variadic one.
func humanReadableArgName(a *argument) string {
	name := a.name
	if a.variadic {
		name += "..."
	}
	if a.required {
		return "<" + name + ">"
	}
	return "[" + name + "]"
}

// optionEntry is one parsed option's value, keyed by its long flag (every
// option in this tree has one) in node.values.
type optionEntry struct {
	set   bool
	value string
}

// actionFunc is a leaf command's handler.
type actionFunc func(rc *runContext) error

// node is one command or command group. It plays the role commander's
// Command class does, restricted to what src/cli/main.ts's tree uses: no
// executable subcommands, hooks, positional-options mode, option choices,
// conflicts or environment variables.
type node struct {
	name        string
	description string
	args        []*argument
	options     []*option // this command's own options, in registration order; never includes helpOption
	version     string    // set on the root only, by setVersion
	children    []*node
	parent      *node
	action      actionFunc // nil for a group command

	// values holds this node's own parsed option values, set during its own
	// parseOptions call. Rebuilt fresh by every cli.Main call — see buildProgram.
	values map[string]optionEntry
}

func newCommand(name, description string) *node {
	return &node{name: name, description: description}
}

func (n *node) addChild(c *node) *node {
	c.parent = n
	n.children = append(n.children, c)
	return n
}

func (n *node) addOption(flags, description string) *node {
	n.options = append(n.options, newOption(flags, description))
	return n
}

func (n *node) addArgument(spec, description string) *node {
	n.args = append(n.args, newArgument(spec, description))
	return n
}

func (n *node) setAction(fn actionFunc) *node {
	n.action = fn
	return n
}

func (n *node) setVersion(version string) *node {
	n.version = version
	n.options = append(n.options, &option{flags: "-V, --version", short: "-V", long: "--version", description: "output the version number", isVersion: true})
	return n
}

// findChild matches a name against this node's children, by name only —
// nothing in this tree defines an alias.
func (n *node) findChild(name string) *node {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// findOption matches arg against this node's own registered options (never
// helpOption — see its doc comment).
func (n *node) findOption(arg string) *option {
	for _, o := range n.options {
		if o.short == arg || o.long == arg {
			return o
		}
	}
	return nil
}

// hasImplicitHelp reports whether this node gets its own "help [command]"
// pseudo-subcommand, the way commander's _getHelpCommand does when nothing
// overrides it: a group with children and no action handler. Every group in
// this tree qualifies, and no leaf does.
func (n *node) hasImplicitHelp() bool {
	return len(n.children) > 0 && n.action == nil
}

// visibleOptions is this node's options with helpOption appended, the order
// help.js's visibleOptions and unknownOption's candidate walk both use.
// Commander's version hides the conflicting flag it would introduce; nothing
// in this tree defines its own -h or --help, so that case does not arise.
func (n *node) visibleOptions() []*option {
	out := make([]*option, 0, len(n.options)+1)
	out = append(out, n.options...)
	out = append(out, helpOption)
	return out
}

// setValue records opt's value on n, initialising the map on first use.
func (n *node) setValue(opt *option, value string) {
	if n.values == nil {
		n.values = make(map[string]optionEntry)
	}
	n.values[opt.long] = optionEntry{set: true, value: value}
}

// setFlag records a boolean opt as present.
func (n *node) setFlag(opt *option) {
	n.setValue(opt, "")
}

// get reads a previously-parsed option value by its long flag, e.g. "--all".
func (n *node) get(long string) optionEntry {
	return n.values[long]
}
