// Package commands implements the CLI's verbs. The CLI parses arguments and
// hands each verb an Env; nothing here reads argv, the process environment or
// the working directory directly, so every verb runs the same in tests.
package commands

import (
	"io"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
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
	// Cwd is the physical working directory.
	Cwd string
	// ConfigPath is --config; empty means the nearest berth.toml above Cwd.
	ConfigPath string
	Runner     run.Runner
	// Lifecycle is the process's signal handling; mutations hold their locks through it.
	Lifecycle *lifecycle.Lifecycle
	// Stdin, Stderr, StdinIsTTY and StderrIsTTY serve the confirmation prompt.
	Stdin       io.Reader
	Stderr      io.Writer
	StdinIsTTY  bool
	StderrIsTTY bool
}

// ListOptions are `worktree list`'s flags.
type ListOptions struct {
	All  bool
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

// NotPorted is what a verb the Go build does not have yet answers with.
func NotPorted(commandName, verb string) error {
	return clierr.WithCode("'"+commandName+" "+verb+"' is not yet ported to the Go build; use the TypeScript berth.", 2)
}
