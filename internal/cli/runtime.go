package cli

import (
	"context"
	"io"
	"os"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// newRunner is the seam internal/run/osrun plugs into once it exists: nil
// here means every leaf action runs with commands.Env.Runner unset. A
// command whose body needs to run a subprocess is not usable until the
// coordinator sets this.
var newRunner func() run.Runner

// runContext is what a leaf action runs with: its own parsed arguments and
// options, plus the commands.Env every ported verb takes.
type runContext struct {
	ctx        context.Context
	env        *commands.Env
	positional []string
	values     map[string]optionEntry
}

// bool reports whether a boolean flag, named by its long form (e.g. "--all"),
// was given.
func (rc *runContext) bool(long string) bool { return rc.values[long].set }

// string is a value option's argument, named by its long form (e.g.
// "--domain"); "" if it was not given.
func (rc *runContext) string(long string) string { return rc.values[long].value }

// arg is the positional argument at i, or "" past the end — every leaf
// command in this tree has at most one, and it is always optional.
func (rc *runContext) arg(i int) string { return at(rc.positional, i) }

// envBuilder returns a parseState.buildEnv closure: everything it closes
// over is fixed for the whole invocation except configPath, which is only
// known once --config has been parsed (see parseState.run).
func envBuilder(commandName, invocation string, console *logger.Console, getenv layout.Getenv, stdin io.Reader, stderr io.Writer, life *lifecycle.Lifecycle) func(configPath string) (*commands.Env, error) {
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
			Cwd:         cwd,
			ConfigPath:  configPath,
			Runner:      runner,
			Lifecycle:   life,
			Stdin:       stdin,
			Stderr:      stderr,
			StdinIsTTY:  stdinIsTTY(),
			StderrIsTTY: stderrIsTTY(),
		}, nil
	}
}
