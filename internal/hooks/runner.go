package hooks

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/progress"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// OutputMode is where a hook's output is streamed.
type OutputMode int

const (
	// Terminal tees both of the hook's streams to this process's own
	// matching stream, exactly as they would appear run directly.
	Terminal OutputMode = iota
	// Stderr tees both streams to this process's stderr instead, for a
	// caller (doctor's verify) whose own stdout is a report the hook's
	// output must not interleave with.
	Stderr
)

// HookContext is what one worktree's hooks need to run: the environment
// (built once, shared with Compose — see worktree.Environment), and where to
// run and log them. Named HookContext, not Context, so a call site holding
// both this and a context.Context — every RunHook and RunLifecycle caller —
// never has to tell them apart by squinting at "ctx" versus "hctx".
type HookContext struct {
	Source worktree.EnvironmentSource
	Ref    worktree.Ref
	// Hooks are the configured hooks; RunCommand does not read them.
	Hooks  config.Hooks
	Logger logger.Logger
	Runner run.Runner
	// Environ is the environment a hook inherits on top of; os.Environ() in
	// production, so a hook that calls out to other tools sees a normal shell.
	Environ []string
	// Routes is this worktree's declared service routes, for
	// worktree.ServiceURLVariables (#28); nil gives a hook neither variable.
	Routes []worktree.ServiceRoute
	// ComposeFile is the absolute path of the project's Compose file; empty
	// leaves COMPOSE_FILE alone. See environment.
	ComposeFile string
	// Shared is the repository's pinned shared-service addresses, for
	// worktree.SharedAddrVariables (docs/design/routing.md, decision 20); nil
	// gives a hook none.
	Shared []worktree.SharedAddr
	// SharedURLs are the routed web UIs of the repository's shared services,
	// for worktree.SharedURLVariables; nil gives a hook none.
	SharedURLs []worktree.SharedURL
}

// RunOptions configures one hook run.
type RunOptions struct {
	// Argument is passed as the hook's first argument — the backup/restore
	// bundle directory, for BundleStages.
	Argument *string
	Output   OutputMode
	// Timeout stops the hook after this long, replacing the hook's own
	// [hooks.timeout] entry; zero leaves that entry in charge, and no entry
	// means no limit.
	Timeout time.Duration
}

// kind is which of the two things this package runs a configured command
// line as: a hook of a lifecycle stage, or one of the project's named
// commands (#94). They share splitting, resolution, the BERTH_* environment
// and the mapping of an exit to an error; a kind holds only what differs,
// and the words a failure message uses.
type kind struct {
	// noun and title are the word in a message: "hook" and "Hook".
	noun, title string
	// own is the one variable that says which stage or command this is, and
	// foreign the other kind's, dropped from what the child inherits: a
	// command that runs `berth up` on another worktree must not hand its
	// hooks a BERTH_COMMAND, nor a hook that runs a command a BERTH_STAGE.
	own     string
	foreign string
}

var (
	hookKind    = kind{noun: "hook", title: "Hook", own: "STAGE", foreign: "BERTH_COMMAND"}
	commandKind = kind{noun: "command", title: "Command", own: "COMMAND", foreign: "BERTH_STAGE"}
)

// environment is the BERTH_* vocabulary Compose receives, plus k's own
// variable set to name (BERTH_STAGE for a hook, BERTH_COMMAND for a
// command), hctx.Routes' URL variables (#28) and hctx.Shared's pinned address
// variables, laid over the inherited environment — never a replacement of
// it, so a hook that calls other tools still sees a normal PATH and HOME.
// Inherited URL and shared address variables are dropped first: a hook of
// another worktree passes its own down, and they must not stand in for
// routes or shared addresses this worktree doesn't have. So is the other
// kind's variable, see kind.
func environment(hctx HookContext, k kind, name string) []string {
	pairs := append(worktree.Environment(hctx.Source, hctx.Ref), [2]string{k.own, name})
	// The worktree's own Compose project (BERTH_COMPOSE_PROJECT, derived once
	// in worktree.Environment), under Compose's own variable too, so a hook's
	// `docker compose exec` reaches this worktree's containers and not the
	// project Compose would name after the directory. COMPOSE_FILE is the
	// configured file, which Compose otherwise looks for as compose.yaml
	// beside berth.toml.
	var composeVars []string
	for _, pair := range pairs {
		if pair[0] == worktree.ComposeProjectKey {
			composeVars = append(composeVars, "COMPOSE_PROJECT_NAME="+pair[1])
		}
	}
	if hctx.ComposeFile != "" {
		composeVars = append(composeVars, "COMPOSE_FILE="+hctx.ComposeFile)
	}
	pairs = append(pairs, worktree.ServiceURLVariables(hctx.Routes)...)
	pairs = append(pairs, worktree.SharedAddrVariables(hctx.Shared)...)
	pairs = append(pairs, worktree.SharedURLVariables(hctx.SharedURLs)...)
	inherited := make([]string, 0, len(hctx.Environ))
	for _, entry := range hctx.Environ {
		key, _, _ := strings.Cut(entry, "=")
		if key == "BERTH_URL" || strings.HasPrefix(key, "BERTH_SERVICE_URL_") || worktree.IsSharedAddrVariable(key) || worktree.IsSharedURLVariable(key) || key == k.foreign {
			continue
		}
		inherited = append(inherited, entry)
	}
	return mergeEnviron(inherited, append(worktree.Variables(pairs), composeVars...)...)
}

// mergeEnviron overlays extra onto environ, later names replacing earlier
// ones, so the child sees exactly one value per name — the rule
// docker.Merge applies for Compose's own children.
func mergeEnviron(environ []string, extra ...string) []string {
	index := map[string]int{}
	merged := make([]string, 0, len(environ)+len(extra))
	for _, entry := range append(append([]string(nil), environ...), extra...) {
		name, _, _ := strings.Cut(entry, "=")
		if i, ok := index[name]; ok {
			merged[i] = entry
			continue
		}
		index[name] = len(merged)
		merged = append(merged, entry)
	}
	return merged
}

// splitPattern is minimal shell-free splitting: whitespace-separated, with
// double-quoted segments kept intact. Hooks needing anything richer should
// be scripts.
var splitPattern = regexp.MustCompile(`"([^"]*)"|(\S+)`)

// splitCommand splits command on whitespace, honoring double-quoted
// segments, including one edge case: a quoted empty segment ("") splits to
// an empty string token, not to nothing.
func splitCommand(command string) []string {
	indices := splitPattern.FindAllStringSubmatchIndex(command, -1)
	parts := make([]string, 0, len(indices))
	for _, m := range indices {
		switch {
		case m[2] != -1: // group 1: a "quoted" segment, possibly empty
			parts = append(parts, command[m[2]:m[3]])
		case m[4] != -1: // group 2: a bare, non-space token
			parts = append(parts, command[m[4]:m[5]])
		default:
			parts = append(parts, "")
		}
	}
	return parts
}

// resolveHookPath is path.resolve(root, executable): executable as given if
// it is already absolute, otherwise joined onto root — never a bare name,
// which resolveCommand instead looks up on the child's own PATH.
func resolveHookPath(root, executable string) string {
	if filepath.IsAbs(executable) {
		return filepath.Clean(executable)
	}
	return filepath.Join(root, executable)
}

// resolveLine splits a configured command line and resolves its executable
// as a hook always has been: relative to root when it starts with "." or
// contains "/", otherwise left as a bare name for the child's own PATH. name
// is the stage or command the line belongs to, for the refusal of an empty
// one.
func resolveLine(k kind, name, line, root string) (executable string, args []string, err error) {
	parts := splitCommand(line)
	// A command that is only whitespace (splitCommand returns no parts at
	// all) and one that is a single empty-string token (a quoted "" as the
	// whole command) are refused the same way.
	if len(parts) == 0 || parts[0] == "" {
		return "", nil, clierr.Newf("%s %q is empty.", k.title, name)
	}
	executable, args = parts[0], slices.Clone(parts[1:])
	if strings.HasPrefix(executable, ".") || strings.Contains(executable, "/") {
		executable = resolveHookPath(root, executable)
	}
	return executable, args, nil
}

// finish turns how a run ended into the error it is reported as: nil for
// exit zero, otherwise a clierr carrying the exit code the process should
// end with — the child's own, 124 for a timeout, 1 for an output overflow —
// and a message naming k's noun and name. A command that could not be
// started keeps the shell's 126 or 127.
func finish(k kind, name, resolved string, timeout time.Duration, result run.Result, runErr error) error {
	if runErr != nil {
		var startErr *run.StartError
		if errors.As(runErr, &startErr) {
			return clierr.WithCode(
				fmt.Sprintf("The %s %s could not be started: `%s` %s", name, k.noun, resolved, startErr.Detail),
				startErr.ExitCode,
			)
		}
		return runErr
	}
	switch {
	case result.TimedOut:
		// 124 is what timeout(1) reports; the child's own exit, if any, was not its verdict.
		return clierr.WithCode(
			fmt.Sprintf("The %s %s timed out after %s and was terminated.", name, k.noun, formatTimeout(timeout)),
			124,
		)
	case result.OutputOverflow:
		return clierr.WithCode(fmt.Sprintf("The %s %s produced more output than can be captured and was stopped.", name, k.noun), 1)
	case result.Signal != 0:
		return clierr.WithCode(fmt.Sprintf("The %s %s was terminated by %s.", name, k.noun, clierr.SignalName(result.Signal)), result.ExitCode)
	case result.ExitCode != 0:
		return clierr.WithCode(fmt.Sprintf("The %s %s failed (exit %d).", name, k.noun, result.ExitCode), result.ExitCode)
	}
	return nil
}

// formatTimeout writes a time limit in the config's own spelling, normalised
// to whole hours, minutes, seconds and milliseconds: 10m, 1h30s, 1m30s and
// 1s500ms, never Go's 10m0s or 1h0m30s.
func formatTimeout(d time.Duration) string {
	var b strings.Builder
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
		if n := d / unit.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, unit.name)
			d -= n * unit.size
		}
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}

// RunHook runs one hook. The command is resolved relative to the project
// root, runs with the project root as its working directory, sees the hook
// environment on top of the parent's, and has its output streamed. Any
// non-zero exit is a failure attributed to the stage — and so, distinctly,
// is a hook that never ran at all or was ended by a signal. Reports whether
// the stage was configured at all.
//
// A hook runs for at most its [hooks.timeout] entry, if it has one
// (opts.Timeout replaces it). When the limit passes, the hook's whole
// process group is stopped by the runner: SIGTERM, then SIGKILL after the
// runner's grace, and Run does not return while anything is left in the
// group. The hook then fails as "timed out", exit 124 (what timeout(1)
// reports), distinct from a non-zero exit of its own. The caller's lock is
// released on the ordinary return path, as for any failed hook.
func RunHook(ctx context.Context, stage config.HookStage, hctx HookContext, opts RunOptions) (bool, error) {
	command := commandFor(hctx.Hooks, stage)
	if command == nil {
		return false, nil
	}
	resolved, args, err := resolveLine(hookKind, string(stage), *command, hctx.Source.ProjectRoot)
	if err != nil {
		return false, err
	}
	if opts.Argument != nil {
		args = append(args, *opts.Argument)
	}
	output := run.Tee
	if opts.Output == Stderr {
		output = run.TeeToStderr
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = hctx.Hooks.TimeoutFor(stage)
	}

	ran := false
	err = progress.WithProgress(hctx.Logger, fmt.Sprintf("Running %s hook", stage), func() error {
		result, runErr := hctx.Runner.Run(ctx, run.Spec{
			Command:      resolved,
			Args:         args,
			Dir:          hctx.Source.ProjectRoot,
			Env:          environment(hctx, hookKind, string(stage)),
			Output:       output,
			Label:        string(stage) + " hook",
			Timeout:      timeout,
			AllowFailure: true,
		})
		if err := finish(hookKind, string(stage), resolved, timeout, result, runErr); err != nil {
			return err
		}
		ran = true
		return nil
	})
	return ran, err
}

// RunCommand runs one of the project's named commands (#94) for `berth run`.
// It is resolved, split and given its environment exactly as a hook is,
// with BERTH_COMMAND in place of BERTH_STAGE, and args appended after
// whatever the configured line already carries. Unlike a hook it runs
// interactively (run.Interactive): in its own process group, holding the
// terminal when berth does, so it can prompt and answer Ctrl-C itself, its
// own output going straight to berth's own streams. RunCommand prints
// nothing itself: no start line and no heartbeat. Its process group is gone
// by the time this returns, the moment the lock the caller may hold can be
// released. hctx.Hooks is not consulted.
//
// The spec never sets InheritLocks: a command that outlived berth by
// holding its lock descriptor would be the opposite of "the lock is
// released once the whole group has exited", and an interactive child's
// group is drained on return anyway (see run.Spec.InheritLocks).
//
// A non-zero exit, a start failure and a death by signal are reported as
// clierr errors carrying the exit code, as for a hook, saying "command".
func RunCommand(ctx context.Context, name string, command config.Command, args []string, hctx HookContext) error {
	resolved, lineArgs, err := resolveLine(commandKind, name, command.Command, hctx.Source.ProjectRoot)
	if err != nil {
		return err
	}
	result, runErr := hctx.Runner.Run(ctx, run.Spec{
		Command:      resolved,
		Args:         append(lineArgs, args...),
		Dir:          hctx.Source.ProjectRoot,
		Env:          environment(hctx, commandKind, name),
		Output:       run.Interactive,
		Label:        name + " command",
		KillAfter:    command.KillAfterDuration(),
		AllowFailure: true,
	})
	return finish(commandKind, name, resolved, 0, result, runErr)
}

// LifecycleOptions configures RunLifecycle.
type LifecycleOptions struct {
	// Stages are which lifecycle stages to run; always in lifecycle order
	// regardless of how they are listed here. Nil means every stage
	// LifecycleOrder returns.
	Stages   []config.HookStage
	Argument *string
	Output   OutputMode
	Timeout  time.Duration
}

// RunLifecycle runs prepare, then seed, then verify, in that order, stopping
// at the first failure. Returns the stages that actually ran (unconfigured
// stages are skipped). A subset of stages still runs in lifecycle order:
// seed cannot precede prepare. `up` asks for prepare and seed; verify is
// ordered here, but only `doctor` runs it, on its own.
func RunLifecycle(ctx context.Context, hctx HookContext, opts LifecycleOptions) ([]config.HookStage, error) {
	order := LifecycleOrder()
	wanted := opts.Stages
	if wanted == nil {
		wanted = order
	}
	wantedSet := make(map[config.HookStage]bool, len(wanted))
	for _, stage := range wanted {
		wantedSet[stage] = true
	}
	var ran []config.HookStage
	for _, stage := range order {
		if !wantedSet[stage] {
			continue
		}
		didRun, err := RunHook(ctx, stage, hctx, RunOptions{Argument: opts.Argument, Output: opts.Output, Timeout: opts.Timeout})
		if err != nil {
			return ran, err
		}
		if didRun {
			ran = append(ran, stage)
		}
	}
	return ran, nil
}
