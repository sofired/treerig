// Package commands implements the CLI's verbs. The CLI parses arguments and
// hands each verb an Env; nothing here reads argv, the process environment or
// the working directory directly, so every verb runs the same in tests.
package commands

import (
	"io"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
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
	// OS is runtime.GOOS, set by the entry point: library code does not ask
	// the runtime, so a test names the OS it means.
	OS string
	// Arch is runtime.GOARCH, set by the entry point for the same reason as
	// OS; together they name the platform a teardown pin is made and checked
	// on.
	Arch string
	// Cwd is the physical working directory.
	Cwd string
	// ConfigPath is --config; empty means the nearest treerig.toml above Cwd.
	ConfigPath string
	// Verbose is -v/--verbose. Nothing reads it yet: the flag is accepted
	// by every command ahead of the verbose output it will switch on
	// (docs/design/cli-1.0.md, "Verbose output").
	//
	// TODO(#205): pass it to the backend, hooks, progress and the logger.
	Verbose bool
	// Progress is --progress, already validated by the command line; the
	// zero value means ProgressAuto. Nothing reads it yet
	// (docs/design/cli-1.0.md, "Rendering").
	//
	// TODO(#208): the entry point's one decision about live or plain output.
	Progress ProgressMode
	Runner   run.Runner
	// Releases downloads a release for `teardown pin --release` and
	// `--latest`: an HTTPS fetcher over the *http.Client cli.MainProcess builds
	// (releasefetch.NewHTTP). Nil from cli.Main and from hand-built test Envs,
	// where a download refuses instead of reaching the network.
	Releases releasefetch.Fetcher
	// PortProbe decides whether a port is free, for allocation and for the
	// check before the router starts; nil asks the operating system
	// (ports.LoopbackProbe). Tests set a fake so they bind nothing.
	PortProbe ports.Probe
	// Listeners says which loopback addresses a host route's port answers
	// on; nil dials (ports.DialListener). Tests set a fake so they bind
	// nothing.
	Listeners ports.ListenerProbe
	// Table identifies and inspects processes for lock recovery's survivor
	// scan (UnlockGlobal, UnlockWithContext); nil defaults to
	// procid.Platform(), the same default lock.Options.Table documents.
	// Tests give it a fake so a scan never reaches this machine's real
	// process table.
	Table procid.Table
	// PasswdHome is the home directory the user database lists for the
	// effective user, looked up by cli.MainProcess; "" when there is none, and
	// from cli.Main and hand-built Envs. It is the one input to the teardown
	// gate's location check that does not come from the environment: HOME can be
	// set by a checkout's environment file, so the exemption for a dotfiles
	// repository at the home directory is granted only when PasswdHome is
	// the same directory as the checked HOME.
	PasswdHome string
	// NoExec says whether the filesystem holding path is mounted noexec, for
	// the warnings of `teardown pin`, `teardown status` and `doctor`; nil asks
	// the operating system (teardown.MountNoExec). Tests set a fake, since no
	// test can mount anything.
	NoExec func(path string) (bool, error)
	// Lifecycle is the process's signal handling; mutations hold their locks through it.
	Lifecycle *lifecycle.Lifecycle
	// Stdin, Stderr, StdinIsTTY and StderrIsTTY serve the confirmation prompt.
	Stdin       io.Reader
	Stderr      io.Writer
	StdinIsTTY  bool
	StderrIsTTY bool
	// Executable is this process's own running binary; os.Executable, but
	// only from cli.MainProcess — cmd/treerig's own entry point, the one
	// caller allowed to let `worktree remove` run its teardown gate (#146)
	// and relocate (#7). cli.Main, which every in-process test calls,
	// always leaves this nil, and so does every Env built by hand for a
	// test; nil skips the gate entirely rather than reading anything.
	Executable func() (string, error)
	// Exec replaces this process's image with the program at path (the
	// pinned teardown binary, or a relocated copy of this one), handing it
	// argv and envv — golang.org/x/sys/unix.Exec, but again only from
	// cli.MainProcess. On success it never returns; a non-nil error means
	// the exec itself failed, which the gate refuses and the relocation
	// survives. Nil skips the gate entirely, the same as a nil Executable —
	// this must never be wired to anything that can replace an in-process
	// test's own image (a go test binary included): see
	// internal/cli's TestEnvBuilderRelocationWiring.
	Exec func(path string, argv, envv []string) error
}

// ProgressMode is how progress is shown: --progress's value.
type ProgressMode string

// The values --progress takes.
const (
	// ProgressAuto redraws on a terminal and prints plain lines elsewhere.
	ProgressAuto ProgressMode = "auto"
	// ProgressPlain prints one line per change and never redraws.
	ProgressPlain ProgressMode = "plain"
)

// ProgressModes are the values --progress takes, in the order help lists them.
var ProgressModes = []ProgressMode{ProgressAuto, ProgressPlain}

// ListOptions are `worktree list`'s flags.
type ListOptions struct {
	All  bool
	JSON bool
}

// OutputOptions are the flags `doctor` and `index` share:
// JSON prints one JSON document on stdout in place of the text form. The
// exit code is the same either way.
type OutputOptions struct {
	JSON bool
	// Version is the running build's version (brand.Version), which `doctor`
	// compares with the teardown pin's release.
	Version string
}

// RemoveOptions are `worktree remove`'s argument and flags.
type RemoveOptions struct {
	// Identifier is a slug or worktree UUID; empty means this checkout.
	Identifier     string
	Domain         string
	LabelNamespace string
	// Yes skips the confirmation phrase.
	Yes bool
	// DevTeardown is --dev-teardown: remove with this build's own code
	// instead of the pinned teardown binary, for work on removal itself. It
	// is a per-invocation flag and nothing else (docs/design/teardown.md,
	// "The developer escape hatch").
	DevTeardown bool
	// Version is the running build's version (brand.Version), which the gate
	// names in its lines and compares with the pin for its freshness hint.
	Version string
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
