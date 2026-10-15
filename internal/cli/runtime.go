package cli

import (
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// newRunner builds the runner every leaf action gets as commands.Env.Runner;
// wiring.go sets it to osrun.New.
var newRunner func() run.Runner

// envBuilder returns a program.buildEnv closure: everything it closes
// over is fixed for the whole invocation except configPath, which is only
// known once --config has been parsed (see program.env). args is argv as
// cli.Main received it, program path included — commands.Env.Args, which
// `worktree remove`'s own relocate-and-reexec (#7) hands unchanged to the
// standalone copy it runs instead. executable and execFn become
// commands.Env's own Executable and Exec unchanged — nil from Main, so
// relocation is always skipped there; real from MainProcess, cmd/berth's
// own entry point, the only caller allowed to let a verb replace this
// process's image.
func envBuilder(commandName, invocation string, console *logger.Console, getenv layout.Getenv, stdin io.Reader, stderr io.Writer, life *lifecycle.Lifecycle, args []string, executable func() (string, error), execFn func(argv0 string, argv, envv []string) error) func(configPath string) (*commands.Env, error) {
	return func(configPath string) (*commands.Env, error) {
		cwd, err := layout.PhysicalWd()
		if err != nil {
			return nil, err
		}
		var runner run.Runner
		if newRunner != nil {
			runner = newRunner()
		}
		return &commands.Env{
			CommandName: commandName,
			Invocation:  invocation,
			Logger:      console,
			Getenv:      getenv,
			Environ:     os.Environ(),
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			Args:        args,
			Cwd:         cwd,
			ConfigPath:  configPath,
			Runner:      runner,
			Lifecycle:   life,
			Stdin:       stdin,
			Stderr:      stderr,
			StdinIsTTY:  stdinIsTTY(),
			StderrIsTTY: stderrIsTTY(),
			Executable:  executable,
			Exec:        execFn,
		}, nil
	}
}

// osExec is commands.Env.Exec's production implementation: it replaces this
// process's image with argv0 (worktree remove's own standalone copy, #7),
// handing it argv and envv unchanged. golang.org/x/sys/unix.Exec wraps the
// same execve(2) internal/lock already relies on surviving fork and exec for
// an inherited lock descriptor. On success it never returns.
func osExec(argv0 string, argv, envv []string) error { return unix.Exec(argv0, argv, envv) }
