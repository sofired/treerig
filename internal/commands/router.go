package commands

// The router verbs, ported from src/cli/main.ts:150-170 and
// src/router/router.ts. One Traefik stack per Docker daemon, its state and
// lock per berth root (#80), so — unlike every other verb in this package —
// it needs no project config file: it is built straight from Env, never
// through Context.
//
// Only `router up` takes the router's lock, and takes it alone (never
// alongside a worktree's own locks): see routerLockScope (index.go) and
// lifecycle.Requests.Router. `router status` and `router down` take no lock
// at all, matching src/router/router.ts, where only ensure() ever wrapped
// itself in withScopedLock.

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/checkoutindex"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/settings"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// routerFromContext builds the shared router's client from cctx's own
// fields — its RouterState, RouterPort, Runner, Logger, Environ, Invocation
// and CommandName, the same values newRouterService resolves from an Env —
// so a verb that already has a CommandContext (DownWithContext,
// RemoveWithContext) needs no Env of its own to reach the router. Unlike
// sharedRouter (Up's own real Router), this is used only for calls that
// take no lock of their own — RemoveWorktreeFragments needs only State, and
// ConnectNetwork/DisconnectNetwork resolve the router's current container
// id fresh on every call; Ensure/Verify are never reached through this path.
// cctx.SharedRouter, when a test has set one, is returned as it is.
func routerFromContext(cctx *CommandContext) (*router.Router, error) {
	if cctx.SharedRouter != nil {
		return cctx.SharedRouter, nil
	}
	return router.New(router.Options{
		State:       cctx.RouterState,
		Port:        cctx.RouterPort,
		Runner:      cctx.Runner,
		Logger:      cctx.Logger,
		Environ:     cctx.Environ,
		Invocation:  cctx.Invocation,
		CommandName: cctx.CommandName,
	})
}

// unrouteWorktree removes ref's dynamic-configuration fragments and
// disconnects the shared router from its own dedicated network
// (backend.RouterNetworkName), before the worktree's own teardown gets a
// chance to remove that network itself: Compose's plain `down` (and Reap's
// own network removal) leaves a network alone only while something is still
// attached to it, and the router staying attached is exactly what would
// otherwise keep the network around forever, or have Compose refuse its
// removal outright (internal/backend's own activeEndpoints tolerance is the
// safety net for the cases this ordering does not fully close).
//
// Called unconditionally by both DownWithContext and RemoveWithContext —
// even for a worktree that was never routed at all, or one the running
// process has no berth.toml for at all (`worktree remove` can target a
// different worktree than the one it runs from) — because both primitives
// it calls tolerate "nothing there" as success on their own.
//
// Fragment removal failing is a hard failure: the fragment is the one thing
// that actually keeps a route live in Traefik (the file provider reads
// nothing else), so "unrouted after down" cannot be promised if removing it
// silently failed. The router's own network disconnect is softer: a
// failure there leaves at most a stale Docker attachment behind — no
// traffic is routed because of it, since the fragment is already gone — so
// it is only logged, and this worktree's own teardown still proceeds.
// Compose then keeps the network while the router is on it, and the
// router's own next Ensure reattaches to it, since it is still labelled; a
// retried `down` or `worktree remove` is what disconnects and removes it.
func unrouteWorktree(ctx context.Context, cctx *CommandContext, routerSvc *router.Router, ref worktree.Ref) error {
	if err := router.RemoveWorktreeFragments(cctx.RouterState, ref.RepositoryID, ref.WorktreeID); err != nil {
		return clierr.Newf("Could not remove this worktree's route fragments: %s", err)
	}
	network := backend.RouterNetworkName(ref.RepositoryID, ref.WorktreeID)
	if err := routerSvc.DisconnectNetwork(ctx, network); err != nil {
		// A cancellation still ends the command; only a Docker failure is
		// downgraded to a warning, for the reason given above. Named as is,
		// plus the recovery, on its own line after Docker's own stderr
		// (DisconnectNetwork's own error already names the router and the
		// network, the same way up.go's own ConnectNetwork call does, but
		// on its own gives no next step — found in review): appending it
		// after a `;` used to land it on whatever line Docker's own,
		// possibly multi-line, stderr happened to end on, rather than
		// reading as its own sentence, and it named no specific action to
		// retry — found in review, round 2 of #55.
		if ctx.Err() != nil {
			return err
		}
		cctx.Logger.Warn(fmt.Sprintf("%s\nA later '%s down' or '%s worktree remove' retries disconnecting this network.", err, cctx.CommandName, cctx.CommandName))
	}
	return nil
}

// newRouterService builds the shared router directly from env, resolving
// and checking BERTH_HOME itself — for a caller (RouterStatus, RouterDown)
// that needs nothing else derived from it. RouterUp and Doctor, which also
// need the control home from the very same berth root, resolve it once via
// checkedHome and call routerServiceFrom directly instead, so neither path
// resolves BERTH_HOME twice (#80, review round 3 item 1).
func newRouterService(env *Env) (*router.Router, layout.RouterLayout, settings.PortResolution, error) {
	home, _, err := checkedHome(env.Getenv)
	if err != nil {
		return nil, layout.RouterLayout{}, settings.PortResolution{}, err
	}
	return routerServiceFrom(env, home)
}

// routerServiceFrom is newRouterService given the berth root already
// checked. The port is settings.ResolveRouterPort's (#79): BERTH_ROUTER_PORT,
// then settings.toml under that same berth root, then 80 — returned with
// where it came from, for `router status` and `doctor` to say.
func routerServiceFrom(env *Env, home string) (*router.Router, layout.RouterLayout, settings.PortResolution, error) {
	state := layout.RouterStateFrom(home)
	resolved, err := settings.ResolveRouterPort(env.Getenv, home)
	if err != nil {
		return nil, layout.RouterLayout{}, settings.PortResolution{}, err
	}
	svc, err := routerServiceWith(env, home, state, resolved)
	if err != nil {
		return nil, layout.RouterLayout{}, settings.PortResolution{}, err
	}
	return svc, state, resolved, nil
}

// routerServiceWith is the shared router built from a berth root, its
// state and a port resolution already made — Context's own, for `up`, so
// one command never resolves BERTH_HOME or reads settings.toml twice (#80,
// #79).
func routerServiceWith(env *Env, home string, state layout.RouterLayout, resolved settings.PortResolution) (*router.Router, error) {
	return router.New(router.Options{
		State:        state,
		Port:         resolved.Port,
		Runner:       env.Runner,
		Logger:       env.Logger,
		Environ:      env.Environ,
		Invocation:   env.Invocation,
		CommandName:  env.CommandName,
		SettingsPath: resolved.SettingsPath,
		// Home is the berth root the caller already checked (checkedHome),
		// stamped on the router's own containers so a later Ensure — from
		// this root or another one — can tell them apart (#82).
		Home: home,
	})
}

// routerHold holds the router's lock alone around action: acquire, run,
// release/abandon and defer signals, the way CommandContext.mutate holds a
// worktree's locks around a mutation. state is always one a caller already
// resolved through resolveRouterState (home.go), never re-derived from raw
// env here: routerLockScope (index.go) builds the same scope from it, the
// one place that does, so there is no second, raw-env path to the router's
// lock target alongside home.go's (#80).
func routerHold(ctx context.Context, env *Env, state layout.RouterLayout, projectName string, action func(context.Context) error) error {
	scope := routerLockScope(state, projectName)
	quiescer, _ := env.Runner.(lifecycle.Quiescer)
	life := env.Lifecycle
	if life == nil {
		life = lifecycle.NewLifecycle(lifecycle.New(), env.Logger)
	}
	return life.Hold(ctx, lifecycle.Env{Logger: env.Logger, Runner: quiescer, Invocation: env.Invocation}, lifecycle.Requests{Router: &scope}, action)
}

// ensureSharedRouter is what `router up` and `up` both do to make sure the
// shared router is up: secure its state directory, then — under the
// router's lock, taken alone — Ensure it. The TypeScript build's
// router.ensure() took its lock itself; here the caller holds it (see
// internal/router's package comment), so both verbs go through this one
// function rather than two copies of the same hold.
func ensureSharedRouter(ctx context.Context, env *Env, svc *router.Router, state layout.RouterLayout) error {
	// Before the router lock is ever taken: lock.New creates <StateDir>/lock
	// and its holder record the moment it is acquired, so a symlinked state
	// directory must be refused first, not found out about via a failed Ensure.
	if err := router.SecureState(state); err != nil {
		return err
	}
	return routerHold(ctx, env, state, svc.ProjectName, func(ctx context.Context) error {
		return svc.Ensure(ctx)
	})
}

// sharedRouter is the Router `up` hands UpWithContext: the real shared
// router, ensured exactly as `router up` ensures it.
type sharedRouter struct {
	env   *Env
	svc   *router.Router
	state layout.RouterLayout
}

// Port returns the shared router's port.
func (r sharedRouter) Port() int { return r.svc.Port }

// Network returns the shared router's Docker network name.
func (r sharedRouter) Network() string { return r.svc.Network }

// ConnectNetwork attaches the shared router to network, delegating to the
// real router.Router.
func (r sharedRouter) ConnectNetwork(ctx context.Context, network string) error {
	return r.svc.ConnectNetwork(ctx, network)
}

// DisconnectNetwork detaches the shared router from network, delegating to
// the real router.Router.
func (r sharedRouter) DisconnectNetwork(ctx context.Context, network string) error {
	return r.svc.DisconnectNetwork(ctx, network)
}

// Ensure brings the shared router up, exactly as `router up` does.
func (r sharedRouter) Ensure(ctx context.Context) error {
	return ensureSharedRouter(ctx, r.env, r.svc, r.state)
}

// RouterUp is `router up`: start the shared router and verify its hardened
// configuration.
func RouterUp(ctx context.Context, env *Env) error {
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return err
	}
	svc, state, _, err := routerServiceFrom(env, home)
	if err != nil {
		return err
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return err
	}
	if err := ensureSharedRouter(ctx, env, svc, state); err != nil {
		return err
	}
	// (#15) Neither the TypeScript build nor Ensure itself ever regenerates
	// the real page: Ensure's own write() writes the placeholder
	// (PlaceholderHTML) only when the index file is missing outright, so the
	// index container's health check has something to fetch before the
	// router's first `compose up --wait` — deleting the page and running
	// `router up` again would otherwise leave that placeholder in place
	// rather than showing every checkout again. Refreshed here, once the
	// router is confirmed up and its own lock released, the same way `up`,
	// `down` and `worktree remove` do (best effort: a stale or still-missing
	// index must not turn a clean router start into a failure).
	//
	// This checkout index is bound to whatever repository env.Cwd happens to
	// be in, only for the "is this row local" distinction a snapshot makes;
	// `router up` needs no project config, and the machine-wide page it
	// writes covers every registered checkout regardless of cwd.
	reg := registry.New(registry.Options{
		ControlHome: controlHome, Root: env.Cwd, Runner: env.Runner, Logger: env.Logger,
		CommandName: env.CommandName, Invocation: env.Invocation, UserHome: userHome,
	})
	index := checkoutindex.NewService(checkoutindex.Options{
		ControlHome: controlHome,
		RouterPort:  svc.Port,
		IndexFile:   state.IndexFile,
		Lock:        routerLockScope(state, svc.ProjectName),
		Registry:    reg, Identities: reg.Identities, IdentityRoot: reg.Identities.Root,
		Runner: env.Runner, Logger: env.Logger, Invocation: env.Invocation,
	})
	index.RefreshBestEffort(ctx, "router up")
	env.Logger.Info(fmt.Sprintf("Router is up on 127.0.0.1:%d (network %s); index at %s.", svc.Port, svc.Network, svc.IndexURL))
	return nil
}

// RouterStatus is `router status`: report whether the shared router is
// running as expected. failed reports whether the caller should exit 1 —
// src/cli/main.ts sets process.exitCode directly rather than throwing, since
// the report below already said everything there is to say on stdout and
// stderr; a caller must not also report a generic failure for this.
func RouterStatus(ctx context.Context, env *Env) (failed bool, err error) {
	svc, _, resolved, err := newRouterService(env)
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
	if len(report.Output) > 0 {
		// Where the port came from (#79): on stdout with the status line it
		// belongs to, and, like that line, not when Docker could not be asked.
		env.Logger.Output(text.Printable(resolved.Describe()))
	}
	for _, line := range report.Errors {
		env.Logger.Error(line)
	}
	return report.Failed, nil
}

// RouterDown is `router down`: stop the shared router; worktree services
// keep running but are no longer routed.
func RouterDown(ctx context.Context, env *Env) error {
	svc, _, _, err := newRouterService(env)
	if err != nil {
		return err
	}
	return svc.Down(ctx)
}
