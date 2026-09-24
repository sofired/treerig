package commands

// The `index` verb (#41), and newCheckoutIndex, which every mutating verb
// that must refresh the page afterward (`down`, `worktree remove`; `up`
// takes an Index the same way it takes a Router, per WP5's own wiring) builds
// its real checkoutindex.Service from.

import (
	"context"

	"github.com/sofired/berth/internal/checkoutindex"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/router"
)

// routerLockScope is the one place that builds the shared router's lock
// scope, so a caller that already has the router's own, resolved
// ProjectName (RouterUp, via newRouterService/router.New) and one that has
// no router.Router to ask (newCheckoutIndex, below, called from `down` and
// `worktree remove`, which touch no router) never hand-build two
// independently-shaped copies of it. projectName "" defaults to
// router.DefaultName, matching router.New's own default for a caller with
// nothing more specific to give. lock.RouterScope builds the same scope
// from an unresolved Getenv, for routerHold; keep the two in step.
func routerLockScope(state layout.RouterLayout, projectName string) lock.Scope {
	if projectName == "" {
		projectName = router.DefaultName
	}
	return lock.Scope{
		Target:      state.LockPath,
		Description: "shared router",
		Markers:     procid.Markers{Project: projectName},
	}
}

// newCheckoutIndex builds the real checkout index Service for cctx's own
// checkout: the machine-wide registry and Git identities it already reads
// from, the shared router's own state directory and lock (never anything
// under ControlHome — see layout.RouterState), and cctx's own runner and
// logger. Unlike RouterUp, this path never builds a router.Router, so it has
// no better answer than router.DefaultName for the lock's own project-name
// marker (routerLockScope's own default) — nothing in this build ever
// configures a different one anyway (see router.go's newRouterService).
func newCheckoutIndex(cctx *CommandContext) *checkoutindex.Service {
	return checkoutindex.NewService(checkoutindex.Options{
		ControlHome:  cctx.ControlHome,
		RouterPort:   cctx.RouterPort,
		IndexFile:    cctx.RouterState.IndexFile,
		Lock:         routerLockScope(cctx.RouterState, ""),
		Registry:     cctx.Registry,
		Identities:   cctx.Registry.Identities,
		IdentityRoot: cctx.Registry.Identities.Root,
		Runner:       cctx.Runner,
		Logger:       cctx.Logger,
		Invocation:   cctx.Invocation,
	})
}

// RefreshIndex is `index`: refresh the machine-wide checkout index and print
// it, the way src/cli/main.ts's own `index` command does.
func RefreshIndex(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	index := newCheckoutIndex(cctx)
	snapshot, err := index.Refresh(ctx)
	if err != nil {
		return err
	}
	cctx.Logger.Output(checkoutindex.FormatIndexText(snapshot, index.URL()))
	return nil
}
