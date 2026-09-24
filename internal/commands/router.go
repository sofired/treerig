package commands

// The router verbs, ported from src/cli/main.ts:150-170 and
// src/router/router.ts. The router is machine-wide, so — unlike every other
// verb in this package — it needs no project config file: it is built
// straight from Env, never through Context.
//
// Only `router up` takes the router's lock, and takes it alone (never
// alongside a worktree's own locks): see lock.RouterScope and
// lifecycle.Requests.Router. `router status` and `router down` take no lock
// at all, matching src/router/router.ts, where only ensure() ever wrapped
// itself in withScopedLock.

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/text"
)

// newRouterService builds the shared router directly from env.
func newRouterService(env *Env) (*router.Router, layout.RouterLayout, error) {
	state, err := layout.RouterState(env.Getenv)
	if err != nil {
		return nil, layout.RouterLayout{}, err
	}
	port, err := layout.ResolveRouterPort(env.Getenv)
	if err != nil {
		return nil, layout.RouterLayout{}, err
	}
	svc, err := router.New(router.Options{
		State:       state,
		Port:        port,
		Runner:      env.Runner,
		Logger:      env.Logger,
		Environ:     env.Environ,
		Invocation:  env.Invocation,
		CommandName: env.CommandName,
	})
	if err != nil {
		return nil, layout.RouterLayout{}, err
	}
	return svc, state, nil
}

// routerHold holds the router's lock alone around action: acquire, run,
// release/abandon and defer signals, the way CommandContext.mutate holds a
// worktree's locks around a mutation.
func routerHold(ctx context.Context, env *Env, projectName string, action func(context.Context) error) error {
	scope, err := lock.RouterScope(env.Getenv, projectName)
	if err != nil {
		return err
	}
	quiescer, _ := env.Runner.(lifecycle.Quiescer)
	life := env.Lifecycle
	if life == nil {
		life = lifecycle.NewLifecycle(lifecycle.New(), env.Logger)
	}
	return life.Hold(ctx, lifecycle.Env{Logger: env.Logger, Runner: quiescer, Invocation: env.Invocation}, lifecycle.Requests{Router: &scope}, action)
}

// RouterUp is `router up`: start the shared router and verify its hardened
// configuration.
func RouterUp(ctx context.Context, env *Env) error {
	svc, state, err := newRouterService(env)
	if err != nil {
		return err
	}
	controlHome, err := layout.ResolveControlHome(env.Getenv, "")
	if err != nil {
		return err
	}
	err = routerHold(ctx, env, svc.ProjectName, func(ctx context.Context) error {
		if err := svc.Ensure(ctx); err != nil {
			return err
		}
		// New to the Go port (#12, #13): hand this control home's
		// TypeScript-era router state over to the now machine-wide one, now
		// that it has verified. Run under the same lock as Ensure, so two
		// homes migrating at once — or two `up` runs racing from the same
		// home — never step on each other's rename. Best effort: a router
		// that just came up clean must not be reported as a failure over
		// cleanup of state nothing needs any more.
		if err := router.MigrateLegacyState(controlHome, state, env.Logger); err != nil {
			env.Logger.Warn(fmt.Sprintf("Could not finish migrating this control home's earlier router state: %s", err))
		}
		return nil
	})
	if err != nil {
		return err
	}
	env.Logger.Info(fmt.Sprintf("Router is up on 127.0.0.1:%d (network %s); index at %s.", svc.Port, svc.Network, svc.IndexURL))
	return nil
}

// RouterStatus is `router status`: report whether the shared router is
// running as expected. failed reports whether the caller should exit 1 —
// src/cli/main.ts sets process.exitCode directly rather than throwing, since
// the report below already said everything there is to say on stdout and
// stderr; a caller must not also report a generic failure for this.
func RouterStatus(ctx context.Context, env *Env) (failed bool, err error) {
	svc, _, err := newRouterService(env)
	if err != nil {
		return false, err
	}
	status, err := svc.Status(ctx)
	if err != nil {
		return false, err
	}
	report := router.StatusReport(status)
	for _, line := range report.Output {
		env.Logger.Output(text.Printable(line))
	}
	for _, line := range report.Errors {
		env.Logger.Error(line)
	}
	return report.Failed, nil
}

// RouterDown is `router down`: stop the shared router; worktree services
// keep running but are no longer routed.
func RouterDown(ctx context.Context, env *Env) error {
	svc, _, err := newRouterService(env)
	if err != nil {
		return err
	}
	return svc.Down(ctx)
}
