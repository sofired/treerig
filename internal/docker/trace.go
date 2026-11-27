package docker

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
)

// traced is a Runner that says each docker command before running it.
type traced struct {
	run.Runner
	say func(string)
}

// Traced returns a Runner that calls say with every command Run starts,
// "docker compose --project-name ... up --detach", and then runs it on inner.
// It is what -v adds around the runner (docs/design/cli-1.0.md, "Verbose
// output").
//
// Only a command Run built is said: Run is the one place that sets both the
// "docker" command and InheritLocks, and a hook, a [commands] entry or git never
// does. The line carries treerig's own arguments and nothing else: not the
// environment, which is not an argument, and nothing after a "--", which
// belongs to whatever follows it. It goes through text.Redact as well.
func Traced(inner run.Runner, say func(string)) run.Runner {
	return &traced{Runner: inner, say: say}
}

// Quiesce hands lifecycle.Quiescer through: the proof that every child has been
// reaped is the inner runner's to give, and a wrapper that hid it would make
// every mutation under -v skip it. An inner runner with no children to prove
// has nothing to wait for.
func (t *traced) Quiesce(ctx context.Context) error {
	if q, ok := t.Runner.(interface{ Quiesce(context.Context) error }); ok {
		return q.Quiesce(ctx)
	}
	return nil
}

// Run says the command if it is one of Run's, then runs it.
func (t *traced) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if spec.Command == "docker" && spec.InheritLocks {
		t.say(CommandLine(spec.Args))
	}
	return t.Runner.Run(ctx, spec)
}

// CommandLine is "docker" and args as one line to read, redacted: arguments
// with spaces or shell characters are quoted, and a "--" ends the line, since
// what follows it is not treerig's. Each argument is redacted before it is
// quoted (text.RedactArguments), so a quoted value stays whole.
func CommandLine(args []string) string {
	if cut := slices.Index(args, "--"); cut >= 0 {
		args = append(slices.Clone(args[:cut]), "--", "...")
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "docker")
	for _, arg := range text.RedactArguments(args) {
		parts = append(parts, quoteIfNeeded(arg))
	}
	return strings.Join(parts, " ")
}

// plainArgument matches an argument a shell reads back as written.
var plainArgument = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./<>-]+$`)

// quoteIfNeeded single-quotes arg unless it is plain.
func quoteIfNeeded(arg string) string {
	if plainArgument.MatchString(arg) {
		return arg
	}
	return text.ShellQuote(arg)
}
