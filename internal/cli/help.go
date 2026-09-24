package cli

// Ported from node_modules/commander/lib/help.js, restricted to what this
// tree exercises: no colour, no custom Help subclass, no help groups beyond
// the implicit "Options:"/"Commands:" ones, no global-options section (the
// tree never turns showGlobalOptions on), and no argument choices or
// defaults (none are used).
//
// (The MIT License)
//
// Copyright (c) 2011 TJ Holowaychuk <tj@vision-media.ca>
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// 'Software'), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED 'AS IS', WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
// IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY
// CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
// TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
// SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

import (
	"regexp"
	"strings"

	"github.com/sofired/berth/internal/text"
)

const minWidthToWrap = 40

// helpPseudoCommand is the synthetic "help [command]" entry every group
// lists among its subcommands and recognises as an operand, the way
// commander's lazily-created _helpCommand does (see node.hasImplicitHelp).
var helpPseudoCommand = newCommand("help", "display help for command").addArgument("[command]", "")

// visibleCommands is help.js's function of the same name: this node's
// children, with the implicit help pseudo-command appended when it has one.
// Nothing in this tree hides a command or sorts subcommands.
func (n *node) visibleCommands() []*node {
	out := make([]*node, 0, len(n.children)+1)
	out = append(out, n.children...)
	if n.hasImplicitHelp() {
		out = append(out, helpPseudoCommand)
	}
	return out
}

// visibleArguments is help.js's function of the same name: every registered
// argument, but only if at least one has a description — none in this tree
// do, so the "Arguments:" section never renders. Kept for fidelity.
func (n *node) visibleArguments() []*argument {
	for _, a := range n.args {
		if a.description != "" {
			return n.args
		}
	}
	return nil
}

// subcommandTerm is help.js's function of the same name. It deliberately
// ignores whether the child itself has subcommands or an implicit help
// option — "[options]" appears only when the child registered an option of
// its own, per the upstream comment this paraphrases.
func subcommandTerm(child *node) string {
	var args []string
	for _, a := range child.args {
		args = append(args, humanReadableArgName(a))
	}
	term := child.name
	if len(child.options) > 0 {
		term += " [options]"
	}
	if len(args) > 0 {
		term += " " + strings.Join(args, " ")
	}
	return term
}

func optionTerm(o *option) string { return o.flags }

func argumentTerm(a *argument) string { return a.name }

func subcommandDescription(child *node) string { return child.description }

func optionDescription(o *option) string { return o.description }

func argumentDescription(a *argument) string { return a.description }

// commandUsage is help.js's function of the same name: every ancestor's
// name, this command's own name, then its usage string.
func commandUsage(n *node) string {
	var names []string
	for ancestor := n.parent; ancestor != nil; ancestor = ancestor.parent {
		names = append([]string{ancestor.name}, names...)
	}
	names = append(names, n.name)
	return strings.Join(names, " ") + " " + usage(n)
}

// usage is command.js's usage(): derived from whether this node has its own
// options, children, or registered arguments. The implicit help option means
// "[options]" always appears — helpOption is only ever disabled by
// .helpOption(false), which this tree never calls.
func usage(n *node) string {
	var parts []string
	parts = append(parts, "[options]")
	if len(n.children) > 0 {
		parts = append(parts, "[command]")
	}
	for _, a := range n.args {
		parts = append(parts, humanReadableArgName(a))
	}
	return strings.Join(parts, " ")
}

// padWidth is help.js's function of the same name: the widest term across
// options, subcommands and arguments, so their descriptions line up.
func padWidth(n *node) int {
	width := 0
	for _, o := range n.visibleOptions() {
		width = max(width, text.JSLen(optionTerm(o)))
	}
	for _, c := range n.visibleCommands() {
		width = max(width, text.JSLen(subcommandTerm(c)))
	}
	for _, a := range n.visibleArguments() {
		width = max(width, text.JSLen(argumentTerm(a)))
	}
	return width
}

var (
	preformattedPattern = regexp.MustCompile(`\n[^\S\r\n]`)
	lineBreakPattern    = regexp.MustCompile(`\r\n|\n`)
	chunkPattern        = regexp.MustCompile(`[\s]*[^\s]+`)
)

// preformatted is help.js's function of the same name: a line break followed
// by whitespace signals hand-wrapped, hand-indented text that boxWrap should
// leave alone. Nothing in this tree's descriptions is multi-line, but the
// check is kept for fidelity.
func preformatted(s string) bool { return preformattedPattern.MatchString(s) }

// boxWrap is help.js's function of the same name: word-wrap at width,
// preserving existing line breaks, skipped entirely below minWidthToWrap.
func boxWrap(s string, width int) string {
	if width < minWidthToWrap {
		return s
	}
	rawLines := lineBreakPattern.Split(s, -1)
	var wrapped []string
	for _, line := range rawLines {
		chunks := chunkPattern.FindAllString(line, -1)
		if chunks == nil {
			wrapped = append(wrapped, "")
			continue
		}
		sumChunks := []string{chunks[0]}
		sumWidth := text.JSLen(chunks[0])
		for _, chunk := range chunks[1:] {
			visibleWidth := text.JSLen(chunk)
			if sumWidth+visibleWidth <= width {
				sumChunks = append(sumChunks, chunk)
				sumWidth += visibleWidth
				continue
			}
			wrapped = append(wrapped, strings.Join(sumChunks, ""))
			next := strings.TrimLeft(chunk, " \t\n\r\v\f")
			sumChunks = []string{next}
			sumWidth = text.JSLen(next)
		}
		wrapped = append(wrapped, strings.Join(sumChunks, ""))
	}
	return strings.Join(wrapped, "\n")
}

// formatItem is help.js's function of the same name: pad the term to
// termWidth, wrap the description to what remains of helpWidth, and indent
// wrapped and continuation lines under the term column.
func formatItem(term string, termWidth int, description string, helpWidth int) string {
	const itemIndent = 2
	indent := strings.Repeat(" ", itemIndent)
	if description == "" {
		return indent + term
	}

	paddedTerm := text.PadEnd(term, termWidth)

	const spacerWidth = 2
	remaining := helpWidth - termWidth - spacerWidth - itemIndent
	formatted := description
	if remaining >= minWidthToWrap && !preformatted(description) {
		wrapped := boxWrap(description, remaining)
		formatted = strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", termWidth+spacerWidth))
	}

	return indent + paddedTerm + strings.Repeat(" ", spacerWidth) + strings.ReplaceAll(formatted, "\n", "\n"+indent)
}

// formatItemList is help.js's function of the same name: a heading and its
// items, followed by a blank line — or nothing at all when there are no items.
func formatItemList(heading string, items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items)+2)
	out = append(out, heading)
	out = append(out, items...)
	out = append(out, "")
	return out
}

// formatHelp is help.js's function of the same name: Usage, description,
// Arguments, Options, Commands, each a titled block separated by a blank
// line, wrapped to helpWidth.
func formatHelp(n *node, helpWidth int) string {
	termWidth := padWidth(n)
	callFormatItem := func(term, description string) string {
		return formatItem(term, termWidth, description, helpWidth)
	}

	output := []string{"Usage: " + commandUsage(n), ""}

	if n.description != "" {
		output = append(output, boxWrap(n.description, helpWidth), "")
	}

	var argItems []string
	for _, a := range n.visibleArguments() {
		argItems = append(argItems, callFormatItem(argumentTerm(a), argumentDescription(a)))
	}
	output = append(output, formatItemList("Arguments:", argItems)...)

	var optItems []string
	for _, o := range n.visibleOptions() {
		optItems = append(optItems, callFormatItem(optionTerm(o), optionDescription(o)))
	}
	output = append(output, formatItemList("Options:", optItems)...)

	var cmdItems []string
	for _, c := range n.visibleCommands() {
		cmdItems = append(cmdItems, callFormatItem(subcommandTerm(c), subcommandDescription(c)))
	}
	output = append(output, formatItemList("Commands:", cmdItems)...)

	return strings.Join(output, "\n")
}
