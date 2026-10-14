package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/worktree"
)

// RunOptions are `run`'s arguments.
type RunOptions struct {
	// Name is the [commands] entry to run; empty when none was given.
	Name string
	// Args are everything after `--`, appended to the command's own line
	// as separate arguments.
	Args []string
}

// Run is `run`: one of the project's named commands (#94), run
// interactively against this registered worktree.
func Run(ctx context.Context, env *Env, opts RunOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return RunWithContext(ctx, cctx, opts)
}

// RunWithContext is `run`, operating directly on a CommandContext.
//
// It needs only a registration: it never starts services, since a command
// against stopped ones fails in its own script with a clearer error than
// berth could give. The command is looked up before the registration is, so
// a bare `run` lists what there is even in a worktree not yet brought up.
//
// A command with lock = true (the default) runs under the worktree's
// operation lock, taken as every mutation takes it: a second `run`, `up`,
// `down` or `worktree remove` waits, naming the command holding it; a
// `run` inside the same worktree's own environment (a hook, or another
// locked command, which would deadlock on it) refuses at once. The
// development lock is not taken, so an `apply` can run during a `dev`
// session. The lock is released once Runner.Run returns, which for an
// interactive child is after its whole process group is gone.
//
// A command with lock = false takes no lock, and so has no nested-invocation
// refusal to make; it is otherwise run identically, its signals caught,
// forwarded and waited out the same way. The lock is trusted, not enforced:
// see docs/hooks.md.
//
// The invocation recorded for the lock's waiters is "berth run <name>",
// without the arguments after `--`: they are the command's own business,
// and may well carry a secret.
func RunWithContext(ctx context.Context, cctx *CommandContext, opts RunOptions) error {
	cfg := cctx.Loaded.Config
	command, ok := cfg.Commands.Get(opts.Name)
	if !ok {
		return noSuchCommand(opts.Name, cfg.Commands)
	}
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)

	hctx := hooks.HookContext{
		Source: worktree.EnvironmentSource{
			ProjectName: cfg.Project.Name,
			// The registered domain, not the config's: a command addresses
			// the hostname that is actually routed, as a hook does.
			Domain:      registration.Domain,
			ControlHome: cctx.ControlHome,
			ConfigFile:  cctx.Loaded.File,
			ProjectRoot: cctx.Loaded.Root,
		},
		Ref: ref, Logger: cctx.Logger, Runner: cctx.Runner, Environ: cctx.Environ, ComposeFile: cctx.ComposeFile(),
		Routes:     routesOf(cfg, ref.Slug, registration.Domain, cctx.RouterPort, registration.Ports, registration.HostRoutes).All(),
		Shared:     sharedAddrs(cctx, ref.RepositoryID),
		SharedURLs: sharedURLs(cctx, ref.RepositoryID, cctx.RouterPort),
	}
	action := func(ctx context.Context) error {
		return hooks.RunCommand(ctx, opts.Name, command, opts.Args, hctx)
	}
	if !command.Lock {
		return action(ctx)
	}
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)
	locking := *cctx
	locking.Invocation = strings.Join([]string{cctx.CommandName, "run", opts.Name}, " ")
	return locking.mutate(ctx, ref, host, false, func(ctx context.Context) error {
		// The lock may have been held by someone else for minutes while this
		// waited: a `worktree remove` can have finished in between, and the
		// command would then act on a worktree that no longer exists.
		found, err := store.Find(ref.WorktreeID)
		if err != nil {
			return err
		}
		if found == nil {
			return clierr.Newf("%s was removed while '%s run %s' was waiting for its lock; run '%s up' first.",
				host, cctx.CommandName, opts.Name, cctx.CommandName)
		}
		return action(ctx)
	})
}

// noSuchCommand is the refusal for a missing or unknown command name: one
// line, `No command "nope": berth.toml defines plan, apply.` (or `has no
// [commands].`), naming the commands berth.toml defines in file order. name
// is empty when none was given, and the line then says "No command given".
func noSuchCommand(name string, commands config.Commands) error {
	what := "No command given"
	if name != "" {
		what = fmt.Sprintf("No command %q", name)
	}
	if len(commands) == 0 {
		return clierr.Newf("%s: %s has no [commands].", what, brand.ConfigFile)
	}
	return clierr.Newf("%s: %s defines %s.", what, brand.ConfigFile, strings.Join(commands.Names(), ", "))
}
