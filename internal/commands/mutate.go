package commands

import (
	"context"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/progress"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/worktree"
)

// mutate runs one mutation of one worktree. The worktree lock is held
// throughout, and SIGINT/SIGTERM are deferred until the action has settled,
// its subprocesses are reaped, and the lock is released — then re-raised. A
// half-finished mutation is never left locked, and never left running.
// exclusive also takes the development-session lock: the verb must not run
// while `dev` is attached.
func (c *CommandContext) mutate(ctx context.Context, ref worktree.Ref, exclusive bool, action func(context.Context) error) error {
	markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
	operation, development := lock.WorktreeScopes(c.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
	requests := lifecycle.Requests{Operation: &operation}
	if exclusive {
		requests.Development = &development
	}
	quiescer, _ := c.Runner.(lifecycle.Quiescer)
	life := c.Lifecycle
	if life == nil {
		life = lifecycle.NewLifecycle(lifecycle.New(), c.Logger)
	}
	return life.Hold(ctx, lifecycle.Env{Logger: c.Logger, Runner: quiescer, Invocation: c.Invocation}, requests, action)
}

// writer names this process to whoever waits on the registry lock.
func (c *CommandContext) writer() registry.Writer {
	return registry.Writer{Logger: c.Logger, Invocation: c.Invocation}
}

// requireRegistration is this checkout's registration, or the advice to bring it up first.
func (c *CommandContext) requireRegistration(ctx context.Context) (*registry.WorktreeRegistration, error) {
	registration, err := c.Registry.Current(ctx)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, clierr.Newf("This worktree is not registered. Run '%s up' first.", c.CommandName)
	}
	return registration, nil
}

// withProgress logs a phase and an elapsed-time heartbeat while it runs. A
// thin alias kept here so this package's own mutations read the way they
// always have; internal/hooks calls internal/progress directly, since it
// cannot import this package (this package will come to import it, for the
// lifecycle hooks in `up`'s body).
func withProgress(log logger.Logger, phase string, action func() error) error {
	return progress.WithProgress(log, phase, action)
}

// Down is `down`.
func Down(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return DownWithContext(ctx, cctx)
}

// DownWithContext stops the worktree's services. Data survives; `worktree
// remove` is the destructive verb. Unlike the TypeScript build, `down` defers
// signals like every other mutation (issue #23).
func DownWithContext(ctx context.Context, cctx *CommandContext) error {
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	runtime := cctx.Runtime(cctx.Loaded.Config.LabelNamespace())
	routerSvc, err := routerFromContext(cctx)
	if err != nil {
		return err
	}
	if err := cctx.mutate(ctx, ref, false, func(ctx context.Context) error {
		// Unroutes before the worktree's own teardown, unconditionally —
		// see unrouteWorktree's own comment for why, and for its
		// hard-fail/best-effort split between fragment removal and the
		// router's own network disconnect.
		if err := unrouteWorktree(ctx, cctx, routerSvc, ref); err != nil {
			return err
		}
		return withProgress(cctx.Logger, "Stopping "+host, func() error { return runtime.Down(ctx, ref) })
	}); err != nil {
		return err
	}
	// Best effort, as src/commands/worktree.ts's own `down` does: a stale
	// index is an inconvenience, a failed `down` because of it would not
	// be. Prints nothing when the refresh succeeds.
	refreshIndexBestEffort(ctx, cctx, "down")
	cctx.Logger.Info(host + " is down.")
	return nil
}
