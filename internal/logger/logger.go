// Package logger writes what the tool says about itself.
//
// Everything the tool says about itself goes to stderr — progress, waiting
// notices, warnings, failures — so `treerig list > worktrees.txt` and
// `treerig list --json | jq` get the output and nothing else. Only
// Output writes to stdout, and only it is unfiltered: a caller passing a
// borrowed value through it runs it through text.Printable first, because only
// the caller knows whether the line is prose, a table column, or exact JSON.
//
// Filtering is per line. Plenty of messages are deliberately several lines —
// a list of config problems, a command's captured stderr, a remedy to copy —
// and the line breaks are this tool's own; what a subprocess or another
// process's lock record put inside a line is what has to be neutralized.
package logger

import (
	"io"
	"strings"
	"sync"

	"github.com/sofired/treerig/internal/text"
)

// Logger is what commands report through. It is an interface so a TUI can
// supply its own later.
type Logger interface {
	// Info is progress and notices, on stderr.
	Info(message string)
	Warn(message string)
	Error(message string)
	// Detail is a line only -v shows, on stderr, run through text.Redact:
	// the commands treerig runs, IDs and paths. Without -v it says nothing.
	Detail(message string)
	// Output is primary command output, unprefixed on stdout so it can be piped.
	Output(message string)
}

// Verbosity is what the command line needs of a logger to hand it -v, which is
// parsed after the logger is made. *Console implements it.
type Verbosity interface {
	SetVerbose(verbose bool)
}

var _ Verbosity = (*Console)(nil)

// Console prefixes human-facing lines with the command name.
type Console struct {
	prefix string
	stdout io.Writer
	stderr io.Writer
	mu     sync.Mutex
	// verbose is -v: whether Detail says anything.
	verbose bool
}

// NewConsole writes notices to stderr and output to stdout.
func NewConsole(commandName string, stdout, stderr io.Writer) *Console {
	return &Console{prefix: "[" + commandName + "]", stdout: stdout, stderr: stderr}
}

// SetVerbose says whether Detail writes (-v). The command line is parsed after
// the console is made, so the flag reaches it here.
func (c *Console) SetVerbose(verbose bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.verbose = verbose
}

// Detail writes "[name] message", redacted, when -v is set, and nothing
// otherwise.
func (c *Console) Detail(message string) {
	c.mu.Lock()
	on := c.verbose
	c.mu.Unlock()
	if on {
		c.say(c.prefix + " " + text.Redact(message))
	}
}

// Info writes "[name] message".
func (c *Console) Info(message string) { c.say(c.prefix + " " + message) }

// Warn writes "[name] WARNING: message".
func (c *Console) Warn(message string) { c.say(c.prefix + " WARNING: " + message) }

// Error writes "[name] ERROR: message".
func (c *Console) Error(message string) { c.say(c.prefix + " ERROR: " + message) }

// Output writes the message and a newline to stdout, unfiltered.
func (c *Console) Output(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.stdout, message+"\n")
}

// Raw writes lines to stderr exactly as given, filtered per line; the debug
// stack trace uses it.
func (c *Console) Raw(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.stderr, text.PrintableLines(message)+"\n")
}

// say filters each line and indents continuations: borrowed text controls
// where its own line breaks are, and an unindented continuation could read as
// a line this tool wrote.
func (c *Console) say(message string) {
	lines := strings.Split(message, "\n")
	for i, line := range lines {
		if i == 0 || line == "" {
			lines[i] = text.Printable(line)
		} else {
			lines[i] = "  " + text.Printable(line)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.stderr, strings.Join(lines, "\n")+"\n")
}

// Silent discards everything.
type Silent struct{}

// Info discards the message.
func (Silent) Info(string) {}

// Warn discards the message.
func (Silent) Warn(string) {}

// Error discards the message.
func (Silent) Error(string) {}

// Detail discards the message.
func (Silent) Detail(string) {}

// Output discards the message.
func (Silent) Output(string) {}

// Recording keeps every line, prefixed info:/warn:/error:/detail:, and every Output
// line in Printed. For tests.
type Recording struct {
	mu      sync.Mutex
	Lines   []string
	Printed []string
	// Verbose records Detail lines, as -v shows them; without it they are
	// dropped. Set it before the recorder is shared, as it is read unlocked.
	Verbose bool
}

// record appends line to Lines, locked so concurrent commands can log safely.
func (r *Recording) record(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Lines = append(r.Lines, line)
}

// Info records the message prefixed "info: ".
func (r *Recording) Info(message string) { r.record("info: " + message) }

// Warn records the message prefixed "warn: ".
func (r *Recording) Warn(message string) { r.record("warn: " + message) }

// Error records the message prefixed "error: ".
func (r *Recording) Error(message string) { r.record("error: " + message) }

// Detail records the message, redacted and prefixed "detail: ", when Verbose is set.
func (r *Recording) Detail(message string) {
	if r.Verbose {
		r.record("detail: " + text.Redact(message))
	}
}

// Output records the message, unprefixed, in Printed.
func (r *Recording) Output(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Printed = append(r.Printed, message)
}

// Snapshot returns copies of the recorded lines, safe to read while commands still log.
func (r *Recording) Snapshot() (lines, printed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.Lines...), append([]string(nil), r.Printed...)
}
