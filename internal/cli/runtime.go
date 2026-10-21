package cli

import (
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/run"
)

// newRunner builds the runner every leaf action gets as commands.Env.Runner;
// wiring.go sets it to osrun.New.
var newRunner func() run.Runner

// envBuilder returns a program.buildEnv closure: everything it closes
// over is fixed for the whole invocation except configPath, which is only
// known once --config has been parsed (see program.env). args is argv as
// cli.Main received it, program path included — commands.Env.Args, which
// `worktree remove`'s teardown gate (#146) hands unchanged to the pinned
// binary it runs instead, and its --dev-teardown relocation (#7) to the
// standalone copy. executable and execFn become
// commands.Env's own Executable and Exec unchanged — nil from Main, so
// the gate and relocation are always skipped there; real from MainProcess, cmd/treerig's
// own entry point, the only caller allowed to let a verb replace this
// process's image. releases becomes commands.Env.Releases the same way: nil
// from Main, so no in-process test reaches the network.
func envBuilder(commandName, invocation string, console *logger.Console, getenv layout.Getenv, stdin io.Reader, stderr io.Writer, life *lifecycle.Lifecycle, args []string, executable func() (string, error), execFn func(argv0 string, argv, envv []string) error, releases releasefetch.Fetcher, passwdHome string) func(configPath string) (*commands.Env, error) {
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
			Releases:    releases,
			PasswdHome:  passwdHome,
		}, nil
	}
}

// osExec is commands.Env.Exec's production implementation: it replaces this
// process's image with the program at path (worktree remove's pinned teardown
// binary, #146, or its standalone copy, #7), handing it argv and envv unchanged. golang.org/x/sys/unix.Exec wraps the
// same execve(2) internal/lock already relies on surviving fork and exec for
// an inherited lock descriptor. On success it never returns.
func osExec(path string, argv, envv []string) error { return unix.Exec(path, argv, envv) }
