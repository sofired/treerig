package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// defaultHelpWidth is the width help wraps to when it is not going to a
// terminal: a pipe, a file, or a test's buffer.
const defaultHelpWidth = 80

// minWrapWidth is the narrowest column worth wrapping text into; with less
// room than this, a line is printed whole and left to the terminal.
const minWrapWidth = 24

// terminalWidth is w's window width when w is a terminal, else
// defaultHelpWidth. Only an *os.File can be a terminal, so a test's buffer
// always gets the default, which keeps the golden corpus at a fixed width
// whatever terminal runs the tests.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return defaultHelpWidth
	}
	size, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 {
		return defaultHelpWidth
	}
	return int(size.Col)
}

// writeHelp writes cmd's help to w, wrapped to w's width.
func writeHelp(cmd *cobra.Command, w io.Writer) {
	_, _ = io.WriteString(w, renderHelp(cmd, terminalWidth(w)))
}

// renderHelp is cobra's default help and usage output for cmd, with every
// description wrapped to width: cobra itself prints each line whole. Its
// sections are the ones this tree uses — description, usage, commands,
// flags, global flags — in cobra's own order and spacing.
func renderHelp(cmd *cobra.Command, width int) string {
	var b strings.Builder
	about := cmd.Long
	if about == "" {
		about = cmd.Short
	}
	if about = strings.TrimRight(about, " \t\n"); about != "" {
		b.WriteString(wrapIndented(about, width, 0) + "\n\n")
	}
	if !cmd.Runnable() && !cmd.HasSubCommands() {
		return b.String()
	}

	b.WriteString("Usage:")
	if cmd.Runnable() {
		b.WriteString("\n  " + cmd.UseLine())
	}
	if cmd.HasAvailableSubCommands() {
		b.WriteString("\n  " + cmd.CommandPath() + " [command]")
		b.WriteString("\n\nAvailable Commands:")
		for _, sub := range cmd.Commands() {
			if !sub.IsAvailableCommand() && sub.Name() != "help" {
				continue
			}
			pad := sub.NamePadding()
			fmt.Fprintf(&b, "\n  %-*s %s", pad, sub.Name(), wrapIndented(sub.Short, width, pad+3))
		}
	}
	if cmd.HasAvailableLocalFlags() {
		b.WriteString("\n\nFlags:\n")
		b.WriteString(wrapFlagUsages(cmd.LocalFlags().FlagUsages(), width))
	}
	if cmd.HasAvailableInheritedFlags() {
		b.WriteString("\n\nGlobal Flags:\n")
		b.WriteString(wrapFlagUsages(cmd.InheritedFlags().FlagUsages(), width))
	}
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintf(&b, "\n\nUse \"%s [command] --help\" for more information about a command.", cmd.CommandPath())
	}
	b.WriteString("\n")
	// A description moved below its name leaves the name's padding behind.
	lines := strings.Split(b.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// wrapFlagUsages rewraps pflag's unwrapped flag listing to width with
// wrapIndented, the same wrapper the rest of help uses. pflag's own wrapping
// breaks lines several columns short of the width it is given, and lets a
// description's last line run to it, so it can neither match the rest of
// help nor be widened safely.
//
// pflag aligns every description in a listing to one column, and a flag's
// own part ("-h, --help", "--config string") never holds two spaces in a
// row, so that column is where the first run of two or more spaces after the
// flag ends.
func wrapFlagUsages(usages string, width int) string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(usages, " \t\n"), "\n") {
		flagStart := len(line) - len(strings.TrimLeft(line, " "))
		gap := strings.Index(line[flagStart:], "  ")
		if gap < 0 {
			out = append(out, line)
			continue
		}
		gap += flagStart
		col := gap + len(line[gap:]) - len(strings.TrimLeft(line[gap:], " "))
		out = append(out, line[:col]+wrapIndented(line[col:], width, col))
	}
	return strings.Join(out, "\n")
}

// wrapIndented word-wraps text to width, for printing from column indent:
// the first line continues whatever the caller already printed, and every
// later line starts with indent spaces. Line breaks already in text are
// kept, a line that starts with a space or tab (an example command, say) is
// kept as it is, and a word longer than a line is never split. When indent leaves
// fewer than minWrapWidth columns, text that does not fit there starts on
// the next line instead, far enough left to have minWrapWidth; a terminal
// narrower than that gets text unwrapped.
func wrapIndented(text string, width, indent int) string {
	next := indent
	if width-next < minWrapWidth {
		next = width - minWrapWidth
	}
	pad := "\n" + strings.Repeat(" ", max(next, 0))
	if next < 0 {
		return strings.ReplaceAll(text, "\n", pad)
	}
	room := width - indent
	if room < minWrapWidth && len(text) > room {
		return pad + wrapIndented(text, width, next)
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if strings.HasPrefix(para, " ") || strings.HasPrefix(para, "\t") {
			out = append(out, para)
			room = width - next
			continue
		}
		// Splitting on single spaces, not strings.Fields, keeps a run of
		// spaces as written wherever the line does not break.
		line := ""
		for _, word := range strings.Split(para, " ") {
			switch {
			case line == "" && word == "":
				continue
			case line == "":
				line = word
			case len(line)+1+len(word) <= room:
				line += " " + word
			default:
				out = append(out, line)
				line = word
				room = width - next
			}
		}
		out = append(out, line)
		room = width - next
	}
	return strings.Join(out, pad)
}
