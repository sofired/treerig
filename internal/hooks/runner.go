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
	Hooks  config.Hooks
	Logger logger.Logger
	Runner run.Runner
	// Environ is the environment a hook inherits on top of; os.Environ() in
	// production, so a hook that calls out to other tools sees a normal shell.
	Environ []string
	// Routes is this worktree's declared service routes, for
	// worktree.ServiceURLVariables (#28); nil gives a hook neither variable.
	Routes []worktree.ServiceRoute
}

// RunOptions configures one hook run.
type RunOptions struct {
	// Argument is passed as the hook's first argument — the backup/restore
	// bundle directory, for BundleStages.
	Argument *string
	Output   OutputMode
	// Timeout stops the hook after this long; zero means no limit.
	Timeout time.Duration
}

// environment is the BERTH_* vocabulary Compose receives, plus this stage's
// own STAGE and hctx.Routes' URL variables (#28), laid over the inherited
// environment — never a replacement of it, so a hook that calls other tools
// still sees a normal PATH and HOME.
func environment(hctx HookContext, stage config.HookStage) []string {
	pairs := append(worktree.Environment(hctx.Source, hctx.Ref), [2]string{"STAGE", string(stage)})
	pairs = append(pairs, worktree.ServiceURLVariables(hctx.Routes)...)
	return mergeEnviron(hctx.Environ, worktree.Variables(pairs)...)
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

// RunHook runs one hook. The command is resolved relative to the project
// root, runs with the project root as its working directory, sees the hook
// environment on top of the parent's, and has its output streamed. Any
// non-zero exit is a failure attributed to the stage — and so, distinctly,
// is a hook that never ran at all or was ended by a signal. Reports whether
// the stage was configured at all.
func RunHook(ctx context.Context, stage config.HookStage, hctx HookContext, opts RunOptions) (bool, error) {
	command := commandFor(hctx.Hooks, stage)
	if command == nil {
		return false, nil
	}
	parts := splitCommand(*command)
	// A command that is only whitespace (splitCommand returns no parts at
	// all) and one that is a single empty-string token (a quoted "" as the
	// whole command) are refused the same way.
	if len(parts) == 0 || parts[0] == "" {
		return false, clierr.Newf("Hook %q is empty.", stage)
	}
	executable, args := parts[0], slices.Clone(parts[1:])
	resolved := executable
	if strings.HasPrefix(executable, ".") || strings.Contains(executable, "/") {
		resolved = resolveHookPath(hctx.Source.ProjectRoot, executable)
	}
	if opts.Argument != nil {
		args = append(args, *opts.Argument)
	}
	output := run.Tee
	if opts.Output == Stderr {
		output = run.TeeToStderr
	}

	ran := false
	err := progress.WithProgress(hctx.Logger, fmt.Sprintf("Running %s hook", stage), func() error {
		result, runErr := hctx.Runner.Run(ctx, run.Spec{
			Command:      resolved,
			Args:         args,
			Dir:          hctx.Source.ProjectRoot,
			Env:          environment(hctx, stage),
			Output:       output,
			Timeout:      opts.Timeout,
			AllowFailure: true,
		})
		if runErr != nil {
			var startErr *run.StartError
			if errors.As(runErr, &startErr) {
				return clierr.WithCode(
					fmt.Sprintf("The %s hook could not be started: `%s` %s", stage, resolved, startErr.Detail),
					startErr.ExitCode,
				)
			}
			return runErr
		}
		switch {
		case result.TimedOut:
			// 124 is what timeout(1) reports; the hook's own exit, if any, was not its verdict.
			return clierr.WithCode(
				fmt.Sprintf("The %s hook timed out after %d ms and was terminated.", stage, opts.Timeout.Milliseconds()),
				124,
			)
		case result.OutputOverflow:
			return clierr.WithCode(fmt.Sprintf("The %s hook produced more output than can be captured and was stopped.", stage), 1)
		case result.Signal != 0:
			return clierr.WithCode(fmt.Sprintf("The %s hook was terminated by %s.", stage, clierr.SignalName(result.Signal)), result.ExitCode)
		case result.ExitCode != 0:
			return clierr.WithCode(fmt.Sprintf("The %s hook failed (exit %d).", stage, result.ExitCode), result.ExitCode)
		}
		ran = true
		return nil
	})
	return ran, err
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
