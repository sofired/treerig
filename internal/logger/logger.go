// Package logger writes what the tool says about itself.
//
// Everything the tool says about itself goes to stderr — progress, waiting
// notices, warnings, failures — so `treerig index > page.txt` and
// `treerig worktree list --json | jq` get the output and nothing else. Only
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
	// Output is primary command output, unprefixed on stdout so it can be piped.
	Output(message string)
}

// Console prefixes human-facing lines with the command name.
type Console struct {
	prefix string
	stdout io.Writer
	stderr io.Writer
	mu     sync.Mutex
}

// NewConsole writes notices to stderr and output to stdout.
func NewConsole(commandName string, stdout, stderr io.Writer) *Console {
	return &Console{prefix: "[" + commandName + "]", stdout: stdout, stderr: stderr}
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

// Output discards the message.
func (Silent) Output(string) {}

// Recording keeps every line, prefixed info:/warn:/error:, and every Output
// line in Printed. For tests.
type Recording struct {
	mu      sync.Mutex
	Lines   []string
	Printed []string
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
