// Package commands implements the CLI's verbs. The CLI parses arguments and
// hands each verb an Env; nothing here reads argv, the process environment or
// the working directory directly, so every verb runs the same in tests.
package commands

import (
	"io"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/text"
)

// Env is everything a verb may take from the process it runs in.
type Env struct {
	// CommandName is what the CLI was invoked as; it appears in every hint.
	CommandName string
	// Invocation is the command line as invoked, arguments space-joined; it
	// names this process to whoever waits on a lock it holds.
	Invocation string
	Logger     logger.Logger
	Getenv     layout.Getenv
	// Environ is the process environment, for children that inherit it; os.Environ() in production.
	Environ []string
	// Args is argv exactly as this process was invoked, args[0] the program
	// path — os.Args in production. `worktree remove` re-execs with this
	// unchanged (Remove, internal/binstore) so the process it hands the
	// destructive verb to presents itself exactly as this one was invoked.
	Args []string
	// Cwd is the physical working directory.
	Cwd string
	// ConfigPath is --config; empty means the nearest berth.toml above Cwd.
	ConfigPath string
	Runner     run.Runner
	// Table identifies and inspects processes for lock recovery's survivor
	// scan (UnlockGlobal, UnlockWithContext); nil defaults to
	// procid.Platform(), the same default lock.Options.Table documents.
	// Tests give it a fake so a scan never reaches this machine's real
	// process table.
	Table procid.Table
	// Lifecycle is the process's signal handling; mutations hold their locks through it.
	Lifecycle *lifecycle.Lifecycle
	// Stdin, Stderr, StdinIsTTY and StderrIsTTY serve the confirmation prompt.
	Stdin       io.Reader
	Stderr      io.Writer
	StdinIsTTY  bool
	StderrIsTTY bool
	// Executable is this process's own running binary; os.Executable, but
	// only from cli.MainProcess — cmd/berth's own entry point, the one
	// caller allowed to let `worktree remove` relocate (#7). cli.Main,
	// which every in-process test calls, always leaves this nil, and so
	// does every Env built by hand for a test; nil skips relocation
	// entirely rather than reading anything.
	Executable func() (string, error)
	// Exec replaces this process's image with argv0, argv and envv —
	// golang.org/x/sys/unix.Exec, but again only from cli.MainProcess. On
	// success it never returns; a non-nil error means the exec itself
	// failed. Nil skips relocation entirely, the same as a nil Executable —
	// this must never be wired to anything that can replace an in-process
	// test's own image (a go test binary included): see
	// internal/cli's TestEnvBuilderRelocationWiring.
	Exec func(argv0 string, argv, envv []string) error
}

// ListOptions are `worktree list`'s flags.
type ListOptions struct {
	All  bool
	JSON bool
}

// OutputOptions are the flags `doctor`, `status`, `urls` and `index` share:
// JSON prints one JSON document on stdout in place of the text form. The
// exit code is the same either way.
type OutputOptions struct {
	JSON bool
}

// RemoveOptions are `worktree remove`'s argument and flags.
type RemoveOptions struct {
	// Identifier is a slug or worktree UUID; empty means this checkout.
	Identifier     string
	Domain         string
	LabelNamespace string
	// Yes skips the confirmation phrase.
	Yes bool
}

// printJSON prints value as the command's one JSON document on stdout, the
// way `worktree list --json` does: indented, and ASCII-only.
func printJSON(cctx *CommandContext, value any) error {
	out, err := text.SafeJSON(value)
	if err != nil {
		return err
	}
	cctx.Logger.Output(out)
	return nil
}
