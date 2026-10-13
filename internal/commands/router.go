package commands

// The router verbs. One Traefik stack per Docker daemon, its state and
// lock per berth root (#80), so — unlike every other verb in this package —
// it needs no project config file: it is built straight from Env, never
// through Context.
//
// Only `router up` takes the router's lock, and takes it alone (never
// alongside a worktree's own locks): see routerLockScope (index.go) and
// lifecycle.Requests.Router. `router status` and `router down` take no lock
// at all: only Ensure ever needs one.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

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

// unrouteWorktree removes ref's route files before the worktree's own
// teardown, so no hostname keeps routing to ports the worktree is about to
// give up. Called unconditionally by both DownWithContext and
// RemoveWithContext — even for a worktree that was never routed at all, or
// one the running process has no berth.toml for at all (`worktree remove`
// can target a different worktree than the one it runs from) — because
// removing nothing is success.
//
// Failing to remove a file is a hard failure: the route file is the one
// thing that actually keeps a route live in Traefik (the file provider reads
// nothing else), so "unrouted after down" cannot be promised if removing it
// silently failed.
func unrouteWorktree(cctx *CommandContext, ref worktree.Ref) error {
	if err := router.RemoveWorktreeFragments(cctx.RouterState, ref.RepositoryID, ref.WorktreeID); err != nil {
		return clierr.Newf("Could not remove this worktree's route files: %s", err)
	}
	return nil
}

// controlHomeRegistrations is router.Registrations over a control home's
// records, read without the registry lock: every registry writer replaces a
// record atomically, so a lock-free read sees a record whole or not at all.
type controlHomeRegistrations struct{ controlHome string }

// Lookup reads repositoryID's record of worktreeID afresh. A repository
// with no directory in this control home is UnknownRepository: the router's
// state is per berth root while registries are per control home, so a route
// file naming it may belong to another control home.
func (c controlHomeRegistrations) Lookup(repositoryID, worktreeID string) (router.Registration, error) {
	if _, err := os.Stat(layout.RepositoryLayout(c.controlHome, repositoryID).RepositoryDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return router.Registration{Status: router.UnknownRepository}, nil
		}
		return router.Registration{}, err
	}
	record, err := registry.NewStore(c.controlHome, repositoryID).Find(worktreeID)
	if err != nil {
		return router.Registration{}, err
	}
	if record == nil {
		return router.Registration{Status: router.Unregistered}, nil
	}
	return router.Registration{Status: router.Registered, Ports: record.Ports, HostRoutes: hostRouteTargets(*record)}, nil
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
// one command never resolves BERTH_HOME or reads the router port twice
// (#80, #79). The control home, the allocation range and the mode override
// come from the same berth root.
func routerServiceWith(env *Env, home string, state layout.RouterLayout, resolved settings.PortResolution) (*router.Router, error) {
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return nil, err
	}
	portRange, err := settings.ResolvePortRange(home)
	if err != nil {
		return nil, err
	}
	mode, err := settings.ResolveRouterMode(home)
	if err != nil {
		return nil, err
	}
	dockerHost, _ := env.Getenv("DOCKER_HOST")
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
		Home:          home,
		ControlHome:   controlHome,
		Registrations: controlHomeRegistrations{controlHome: controlHome},
		PortRange:     portRange,
		Probe:         env.PortProbe,
		ModeOverride:  mode,
		DockerHost:    dockerHost,
		ClientOS:      env.OS,
	})
}

// routerServiceForDown is the shared router as `router down` needs it: its
// state and berth root, but not its port. Down acts by project name alone,
// so it neither reads settings.toml nor validates BERTH_ROUTER_PORT — a
// malformed setting must never keep the router from being stopped (found
// in review of #79); the default port stands in, unused.
func routerServiceForDown(env *Env) (*router.Router, error) {
	home, _, err := checkedHome(env.Getenv)
	if err != nil {
		return nil, err
	}
	return router.New(router.Options{
		State:       layout.RouterStateFrom(home),
		Port:        layout.DefaultRouterPort,
		Runner:      env.Runner,
		Logger:      env.Logger,
		Environ:     env.Environ,
		Invocation:  env.Invocation,
		CommandName: env.CommandName,
		Home:        home,
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
// router's lock, taken alone — Ensure it. The caller holds the lock, not
// Ensure itself (see internal/router's package comment), so both verbs go
// through this one function rather than two copies of the same hold.
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

// Ensure brings the shared router up, exactly as `router up` does.
func (r sharedRouter) Ensure(ctx context.Context) error {
	return ensureSharedRouter(ctx, r.env, r.svc, r.state)
}

// RouterUp is `router up`: start the shared router and wait until it is
// healthy.
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
	// (#15) Ensure itself never regenerates the real page: its own write()
	// writes the placeholder
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
	env.Logger.Info(fmt.Sprintf("Router is up on 127.0.0.1:%d%s; index at %s.", svc.Port, routerModeNote(ctx, svc), svc.IndexURL))
	return nil
}

// routerModeNote is " (host mode)" or " (bridge mode)" for the router that
// just started, read off its container; "" when it cannot be read, which is
// no reason to fail a start that succeeded.
func routerModeNote(ctx context.Context, svc *router.Router) string {
	running, err := svc.Running(ctx)
	if err != nil || running.Mode == "" {
		return ""
	}
	return fmt.Sprintf(" (%s mode)", running.Mode)
}

// RouterStatus is `router status`: report whether the shared router is
// running as expected. failed reports whether the caller should exit 1
// directly, rather than through an error, since the report below already
// said everything there is to say on stdout and stderr; a caller must not
// also report a generic failure for this.
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
	svc, err := routerServiceForDown(env)
	if err != nil {
		return err
	}
	return svc.Down(ctx)
}
