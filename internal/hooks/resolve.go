package hooks

import (
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/run"
)

// Resolution is what resolving one configured hook's command found: the
// executable RunHook's own Runner would be handed, and its arguments.
// Command is an absolute path for a hook resolved relative to root or found
// on PATH; for a hook that failed to resolve, it is the executable
// RunHook would have tried instead — resolveHookPath's own output for a
// "." or "/" command, the bare name otherwise — so a caller can still name
// what it looked for.
type Resolution struct {
	Command string
	Args    []string
}

// ResolveHook resolves stage's configured command exactly as RunHook does —
// splitCommand, then resolveHookPath for a command starting with "." or
// containing "/", otherwise a bare name left for env's own PATH to search —
// and predicts what running it would find, without spawning anything:
// run.PredictStart's own verdict, the same 127-for-missing/126-for-not-
// executable-or-not-a-valid-executable-format a real RunHook would reach
// only once it actually tried. root doubles as the working directory
// RunHook itself gives the hook (hctx.Source.ProjectRoot), which
// run.PredictStart needs too: a relative "#!" interpreter resolves against
// it, not PATH, exactly as execve(2) would. env is the hook's own
// environment (typically HookContext.Environ); nil reads this process's
// own PATH, exactly as run.PredictStart documents.
//
// Reports false when stage is not configured at all — nothing for a caller
// (doctor) to check. A configured command that is empty, or made only of
// whitespace, resolves to the same clierr error RunHook itself refuses it
// with, rather than a run.StartError, since there is no exit code to
// predict for a hook that was never a command to begin with.
func ResolveHook(hooksCfg config.Hooks, stage config.HookStage, root string, env []string) (Resolution, bool, error) {
	command := commandFor(hooksCfg, stage)
	if command == nil {
		return Resolution{}, false, nil
	}
	resolution, err := predict(hookKind, string(stage), *command, root, env)
	return resolution, true, err
}

// ResolveCommand is ResolveHook for one of the project's named commands
// (#94): the same splitting, path resolution and prediction, so `doctor`'s
// verdict on a command can never drift from what `berth run` finds. An
// empty command line resolves to the same clierr error RunCommand itself
// refuses it with.
func ResolveCommand(name string, command config.Command, root string, env []string) (Resolution, error) {
	return predict(commandKind, name, command.Command, root, env)
}

// predict is what ResolveHook and ResolveCommand share: resolveLine, then
// run.PredictStart's verdict on the result.
func predict(k kind, name, line, root string, env []string) (Resolution, error) {
	target, args, err := resolveLine(k, name, line, root)
	if err != nil {
		return Resolution{}, err
	}
	resolved, startErr := run.PredictStart(target, root, env)
	if startErr != nil {
		return Resolution{Command: target, Args: args}, startErr
	}
	return Resolution{Command: resolved, Args: args}, nil
}
