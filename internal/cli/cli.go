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
	"github.com/sofired/berth/internal/run"
)

// startLifecycle is the process's one signal handler: the first SIGINT, SIGTERM or
// SIGHUP cancels the context every command runs with, mutations defer it
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
	// Carried so a bounded wait deep under a command — an interactive
	// child's own process-group drain, say — can select on the second
	// SIGINT or SIGTERM (never SIGHUP) without this package or internal/run/osrun
	// importing each other; see internal/run/repeat.go.
	ctx := run.WithRepeated(life.Context(), life.Repeated())

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
	// Stopped before Finish can call Exit: a signal landing in the gap
	// between here and Exit's own signal.Reset is simply absorbed, this
	// process's own Notify no longer listening for it, rather than left
	// queued for something that would otherwise never read it — acceptable
	// since Exit is about to re-raise whatever signal is already pending.
	life.Stop()
	// --version is reported through *exitCode/root itself, not an error a
	// signal death or Finish's own report should ever see.
	if err != nil && errors.Is(err, errVersionRequested) {
		return 0
	}
	// Finish reports err unless it is nothing more than the plain fact
	// that a signal interrupted this process, and, if a signal was
	// received and it is that plain, ends the process by it (never
	// returning) — see its own doc for why: an interrupted command dying
	// by the signal that interrupted it, once its locks are released, is
	// not itself reported as a failure, but an error that carries more
	// (an interactive child's own process-group drain that timed out or
	// was abandoned by a second signal, say) must still reach the user.
	// Called even for a nil err, so a signal received during an otherwise
	// clean run still ends this process by it.
	code := life.Finish(err, func(e error) { report(console, getenv, e) })
	if err != nil {
		return code
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
	var ce *clierr.Error
	if errors.As(err, &ce) {
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
