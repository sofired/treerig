// Package cli is the command line: argument parsing, help rendering and
// error reporting for the berth binary, built on spf13/cobra.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
)

// startLifecycle is the process's one signal handler: the first SIGINT or
// SIGTERM cancels the context every command runs with, mutations defer it
// until their locks are safely released, and Main re-raises it at the end so
// the process dies by it, as a shell expects. Main stops it when the command
// finishes without a signal, so repeated calls leave nothing listening.
var startLifecycle = func(log logger.Logger) *lifecycle.Lifecycle {
	life := lifecycle.NewLifecycle(lifecycle.New(), log)
	life.Start()
	return life
}

// Main is the CLI's entry point for every caller except cmd/berth's own
// main(): parse args, dispatch to the matching verb, and report whatever
// error it returns through a single top-level catch. args is the full argv
// with args[0] the program path — the way os.Args is shaped.
//
// It never lets a verb replace this process's image: commands.Env.Executable
// and .Exec are left nil, so `worktree remove`'s own relocate-and-reexec
// (#7) always skips relocation here. This is deliberate, not an oversight —
// Main is what every in-process test calls (golden_test.go and
// wiring_test.go among them), and any of those may be the process running
// `worktree remove` in-process. A go test binary handed a real Exec would
// replace itself with a copy of itself and re-run with argv `worktree
// remove --yes` — no -test.run filter — which restarts the whole suite in
// place rather than the real berth CLI; #7's own fix shipped this bug once
// already; see MainProcess and internal/cli's relocation tests.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv layout.Getenv) int {
	return runMain(args, stdin, stdout, stderr, getenv, nil, nil)
}

// MainProcess is Main for the one caller allowed to hand `worktree remove`
// the primitives that replace this process's image: cmd/berth's own main().
// Only there is "this process" guaranteed to be a real berth build rather
// than a go test binary, or anything else that happens to link this
// package — the same "only the entry point reads the environment" spirit
// AGENTS.md asks of everything else this build reads from its process.
func MainProcess(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv layout.Getenv) int {
	return runMain(args, stdin, stdout, stderr, getenv, os.Executable, osExec)
}

// runMain is Main and MainProcess's shared body; executable and execFn are
// commands.Env's own Executable and Exec (nil from Main, real from
// MainProcess) — see envBuilder.
func runMain(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv layout.Getenv, executable func() (string, error), execFn func(argv0 string, argv, envv []string) error) int {
	var programPath string
	if len(args) > 0 {
		programPath = args[0]
	}
	rest := args
	if len(args) > 0 {
		rest = args[1:]
	}

	commandName := brand.CLIName(programPath)
	console := logger.NewConsole(commandName, stdout, stderr)
	// What a waiter on one of this process's locks is told it is waiting
	// for: the command name plus the raw arguments, exactly as invoked.
	invocation := commandName
	if len(rest) > 0 {
		invocation = commandName + " " + strings.Join(rest, " ")
	}

	life := startLifecycle(console)
	ctx := life.Context()

	exitCode := new(int)
	p := &program{
		buildEnv: envBuilder(commandName, invocation, console, getenv, stdin, stderr, life, args, executable, execFn),
		exitCode: exitCode,
	}
	root := buildProgram(commandName, brand.Version, p, stdout, stderr)
	if rest == nil {
		rest = []string{}
	}
	root.SetArgs(rest)
	root.SetContext(ctx)

	err := root.Execute()
	// An interrupted command dies by the signal that interrupted it, once its
	// locks are released; what it was doing is not reported as a failure.
	if life.Pending() != 0 {
		life.Exit()
	}
	life.Stop()
	if err != nil {
		if errors.Is(err, errVersionRequested) {
			return 0
		}
		report(console, getenv, err)
		return clierr.Code(err)
	}
	return *exitCode
}

// report prints an expected failure (clierr.Expected)'s message and, if it
// hid others, its cause and each suppressed error; anything else
// additionally hints at BERTH_DEBUG unless it is already set. With debug on,
// the error's cause chain is printed via Logger.Raw either way (see
// errorChain).
func report(console *logger.Console, getenv layout.Getenv, err error) {
	console.Error(err.Error())
	cause := errors.Unwrap(err)
	if cause != nil {
		console.Error("  caused by: " + cause.Error())
	}
	if ce, ok := err.(*clierr.Error); ok {
		for _, suppressed := range ce.Suppressed {
			if suppressed == cause {
				continue
			}
			console.Error("  also: " + suppressed.Error())
		}
	}

	debug := isDebug(getenv)
	if !clierr.Expected(err) && !debug {
		console.Error("  (set " + brand.EnvPrefix + "_DEBUG=1 to see the stack trace)")
	}
	if chain := errorChain(err); debug && chain != "" {
		console.Raw(chain)
	}
}

// isDebug is BERTH_DEBUG, trimmed and compared case-insensitively against
// four accepted spellings.
func isDebug(getenv layout.Getenv) bool {
	raw, _ := getenv(brand.EnvPrefix + "_DEBUG")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// errorChain is what BERTH_DEBUG prints in place of a stack trace: err's
// Unwrap chain, one line per link with its Go type — still filtered the way
// logger.Console.Raw filters everything else this tool prints. An expected
// failure with no cause is its message alone, already printed, so it adds
// nothing and prints nothing.
func errorChain(err error) string {
	if clierr.Expected(err) && errors.Unwrap(err) == nil {
		return ""
	}
	var lines []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		lines = append(lines, fmt.Sprintf("%T: %s", e, e.Error()))
	}
	return strings.Join(lines, "\n")
}
