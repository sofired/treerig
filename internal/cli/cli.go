package cli

import (
	"errors"
	"fmt"
	"io"
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

// Main is the CLI's entry point: parse args, dispatch to the matching verb,
// and report whatever it returns the way src/cli/main.ts's own top-level
// catch does. args is the full argv with args[0] the program path — the way
// os.Args is shaped — so cmd/berth/main.go calls Main(os.Args, ...).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv layout.Getenv) int {
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

	root := buildProgram(commandName, brand.Version)
	ps := &parseState{
		ctx:      ctx,
		stdout:   stdout,
		stderr:   stderr,
		outWidth: terminalWidth(stdout),
		errWidth: terminalWidth(stderr),
		root:     root,
		buildEnv: envBuilder(commandName, invocation, console, getenv, stdin, stderr, life),
	}

	code, err := ps.run(root, nil, rest)
	// An interrupted command dies by the signal that interrupted it, once its
	// locks are released; what it was doing is not reported as a failure.
	if life.Pending() != 0 {
		life.Exit()
	}
	life.Stop()
	if err != nil {
		report(console, getenv, err)
		return clierr.Code(err)
	}
	return code
}

// report is src/cli/main.ts's report() plus the BERTH_DEBUG handling from
// its final catch: an expected failure (clierr.Expected) prints its message
// and, if it hid others, its cause and each suppressed error; anything else
// additionally hints at BERTH_DEBUG unless it is already set. With debug on,
// the error's cause chain is printed via Logger.Raw either way — the
// accepted difference from a JavaScript stack trace, which Go does not have.
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
// the same four spellings src/cli/main.ts accepts.
func isDebug(getenv layout.Getenv) bool {
	raw, _ := getenv(brand.EnvPrefix + "_DEBUG")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// errorChain is the accepted difference from src/cli/main.ts's
// `console.error(printableLines(error.stack ?? error.message))`: Go has no
// stack trace, so BERTH_DEBUG instead prints err's Unwrap chain, one line per
// link with its Go type — still filtered the way logger.Console.Raw filters
// everything else this tool prints. An expected failure with no cause is its
// message alone, already printed, so it adds nothing and prints nothing.
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
