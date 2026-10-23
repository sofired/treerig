package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/progress"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/worktree"
)

// mutate runs one mutation of one worktree. The worktree lock is held
// throughout, and SIGINT, SIGTERM and SIGHUP are deferred until the action has settled,
// its subprocesses are reaped, and the lock is released — then re-raised. A
// half-finished mutation is never left locked, and never left running.
// exclusive also takes the development-session lock: the verb must not run
// while `dev` is attached. host names ref in the nested-invocation refusal,
// as the caller already has it from layout.WorktreeHost.
func (c *CommandContext) mutate(ctx context.Context, ref worktree.Ref, host string, exclusive bool, action func(context.Context) error) error {
	if err := c.refuseOwnWorktreeLock(ref.WorktreeID, host); err != nil {
		return err
	}
	markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
	operation, development := lock.WorktreeScopes(c.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
	requests := lifecycle.Requests{Operation: &operation}
	if exclusive {
		requests.Development = &development
	}
	return c.hold(ctx, requests, action)
}

// mutateSession is mutate for the first half of a development session
// (`dev`, #5): the operation lock and the development-session lock are taken
// together, in that order, and action runs under both. Once it has succeeded
// the operation lock is released, so `down`, `up` and the like can run during
// the session, and session runs holding the development lock alone, until it
// returns; only then is the development lock released. A `worktree remove`,
// which takes the development lock, refuses for exactly that long.
func (c *CommandContext) mutateSession(ctx context.Context, ref worktree.Ref, host string, action, session func(context.Context) error) error {
	if err := c.refuseOwnWorktreeLock(ref.WorktreeID, host); err != nil {
		return err
	}
	markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
	operation, development := lock.WorktreeScopes(c.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
	requests := lifecycle.Requests{Operation: &operation, Development: &development}
	return c.lifecycle().HoldThen(ctx, c.lifecycleEnv(), requests, action, session)
}

// lifecycle is the process's lifecycle, or one that never listens when the
// context has none (a test's).
func (c *CommandContext) lifecycle() *lifecycle.Lifecycle {
	if c.Lifecycle == nil {
		return lifecycle.NewLifecycle(lifecycle.New(), c.Logger)
	}
	return c.Lifecycle
}

// lifecycleEnv is what lifecycle needs from this process to hold locks.
func (c *CommandContext) lifecycleEnv() lifecycle.Env {
	quiescer, _ := c.Runner.(lifecycle.Quiescer)
	return lifecycle.Env{Logger: c.Logger, Runner: quiescer, Invocation: c.Invocation}
}

// hold runs action with requests' locks held, in the fixed order.
func (c *CommandContext) hold(ctx context.Context, requests lifecycle.Requests, action func(context.Context) error) error {
	return c.lifecycle().Hold(ctx, c.lifecycleEnv(), requests, action)
}

// nest runs action with requests' locks taken beside the ones ctx already
// holds, which must be a worktree's operation lock: the shared lock for a
// connect, taken second (docs/design/routing.md, decision 18).
func (c *CommandContext) nest(ctx context.Context, requests lifecycle.Requests, action func(context.Context) error) error {
	return c.lifecycle().Nest(ctx, c.lifecycleEnv(), requests, action)
}

// refuseOwnWorktreeLock refuses, before any wait, to take worktreeID's
// operation lock from inside that worktree's own environment. `up` holds
// the lock while its hooks run, and so does `run` for a command with
// lock = true (#94); neither passes it down, so a hook or command that runs
// `up` or `down` on its own worktree would otherwise wait out the whole lock
// timeout on its own caller (#102, #108). A different worktree's ID, or
// none, passes: a hook may still drive another worktree. host names the
// worktree in the refusal, as its caller renders it (`<slug>.<domain>`).
func (c *CommandContext) refuseOwnWorktreeLock(worktreeID, host string) error {
	if c.insideWorktree(worktreeID) {
		return ownWorktreeLockRefusal(c.CommandName, host)
	}
	return nil
}

// insideWorktree reports whether TREERIG_WORKTREE_ID in this process's
// environment names worktreeID, as it does inside that worktree's hooks.
func (c *CommandContext) insideWorktree(worktreeID string) bool {
	env, ok := c.environWorktreeID()
	return ok && env == worktreeID
}

// ownWorktreeLockRefusal is the nested-invocation refusal, naming the
// worktree by host.
func ownWorktreeLockRefusal(commandName, host string) error {
	return clierr.Newf(
		"This %s invocation runs inside %s's own hooks or commands, which may hold its lock, so it would wait on itself. "+
			"A hook or command can change other worktrees, but not its own.", commandName, host)
}

// writer names this process to whoever waits on the registry lock.
func (c *CommandContext) writer() registry.Writer {
	return registry.Writer{Logger: c.Logger, Invocation: c.Invocation, UserHome: c.UserHome}
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

// Down is `down`.
func Down(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return DownWithContext(ctx, cctx)
}

// DownWithContext stops the worktree's services. Data survives; `worktree
// remove` is the destructive verb. `down` defers signals like every other
// mutation (issue #23).
//
// It tells what happened in its own words. A worktree that is not registered,
// and one with nothing left in Docker, are "Nothing to stop" and exit 0 (the
// decided design, docs/design/cli-1.0.md, "Everything else"). One
// with only stopped containers or a network left is cleaned up through
// `compose down` all the same, and also reported as not running: nothing was
// stopped. Compose's own transcript is shown only with -v.
func DownWithContext(ctx context.Context, cctx *CommandContext) error {
	registration, err := cctx.Registry.Current(ctx)
	if err != nil {
		return err
	}
	if registration == nil {
		cctx.Logger.Info("This worktree is not registered. Nothing to stop.")
		return nil
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	// What Compose last said, for the heartbeat to carry.
	var latest progress.Latest
	runtime := cctx.RuntimeFor(cctx.Loaded.Config.LabelNamespace(), &registration.RegisteredWorktree, cctx.RouterPort)
	runtime = runtime.Observing(latest.Set)
	wasRunning := false
	if err := cctx.mutate(ctx, ref, host, false, func(ctx context.Context) error {
		// Unroutes before the worktree's own teardown, unconditionally —
		// see unrouteWorktree's own comment for why.
		if err := unrouteWorktree(cctx, ref); err != nil {
			return err
		}
		// Shared services are disconnected first: `compose down` leaves a
		// network that something is attached to (decision 22). This needs the
		// operation lock held here and no other.
		if err := disconnectShared(ctx, cctx, ref, cctx.Loaded.Config.LabelNamespace(), cctx.Loaded.Config.LabelNamespace()); err != nil {
			return err
		}
		// Asked before anything is announced: "Stopping" is said only when
		// something runs. The question has a deadline of its own.
		left, err := runtime.Leftover(ctx, ref)
		if errors.Is(err, backend.ErrLeftoverTimeout) {
			return clierr.WithCode(fmt.Sprintf("Docker didn't answer within %s while checking what is left of %s. Run '%s doctor' to see why.",
				runtime.ProbeTimeout(), host, cctx.CommandName), 124)
		}
		if err != nil {
			return err
		}
		switch {
		case left.Running:
			wasRunning = true
			return cctx.phase(progress.PhaseSpec{Verb: "Stopping", What: host, Note: latest.Get}, func() error {
				return runtime.Down(ctx, ref)
			})
		case left.Any():
			// Nothing runs, but a stopped container or a network is left:
			// Compose removes it, and treerig says only that nothing ran.
			cctx.Logger.Detail(fmt.Sprintf("%s has no running service; removing what is left of its Compose project (containers: %t, networks: %t).", host, left.Containers, left.Networks))
			return runtime.Down(ctx, ref)
		}
		return nil
	}); err != nil {
		return err
	}
	// Best effort: a stale index is an inconvenience, a failed `down`
	// because of it would not be. Prints nothing when the refresh succeeds.
	refreshOverviewBestEffort(ctx, cctx, "down")
	if !wasRunning {
		cctx.Logger.Info(host + " is not running. Nothing to stop.")
		return nil
	}
	cctx.Logger.Info(host + " is down.")
	return nil
}
