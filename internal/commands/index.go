package commands

// The `index` verb (#41), and newCheckoutIndex, which every mutating verb
// that must refresh the page afterward (`down`, `worktree remove`; `up`
// takes an Index the same way it takes a Router, per WP5's own wiring) builds
// its real checkoutindex.Service from.

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/router"
)

// routerLockScope is the one place in this build that builds the shared
// router's lock scope, always from an already-resolved layout.RouterLayout
// (a caller's own state, itself only ever reached through resolveRouterState
// in home.go, which applies files.AssertSafeHome to BERTH_HOME first) —
// never re-derived from a raw Getenv, so there is no second, unchecked path
// to the router's lock target (#80). Every caller that takes the router's
// lock (routerHold, unlock.go's router branch, and doctor.go's own report of
// it) goes through this one function: a caller that already has the
// router's own, resolved ProjectName (RouterUp, via
// newRouterService/router.New) and one that has no router.Router to ask
// (newCheckoutIndex, below, called from `down` and `worktree remove`;
// commands.UnlockGlobal, #21) never hand-build two independently-shaped
// copies of it. projectName "" resolves through router.ResolveProjectName(),
// the exact function router.New's own default calls too, rather than naming
// router.DefaultName here a second time — so a caller with no Router built
// yet to ask can never drift onto a different default than New would have
// resolved.
func routerLockScope(state layout.RouterLayout, projectName string) lock.Scope {
	if projectName == "" {
		projectName = router.ResolveProjectName()
	}
	return lock.Scope{
		Target:      state.LockPath,
		Description: "shared router",
		Markers:     procid.Markers{Project: projectName},
		HomeEnvVar:  brand.HomeEnv,
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
//
// router.SecureState is called first, the way RouterUp calls it before ever
// taking the router lock (router.go:78): a symlinked router state directory
// must be refused before this path creates the lock file or writes the
// index page into whatever the symlink points at — RouterUp's own "refuses a
// symlinked state directory" test is exactly this check; newCheckoutIndex
// used to skip it entirely.
func newCheckoutIndex(cctx *CommandContext) (*checkoutindex.Service, error) {
	if err := router.SecureState(cctx.RouterState); err != nil {
		return nil, err
	}
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
	}), nil
}

// refreshIndexBestEffort is what down and worktree remove call instead of
// newCheckoutIndex directly: a refresh is best-effort everywhere else in
// this build (RefreshBestEffort's own doc comment), and newCheckoutIndex
// failing — a symlinked router state directory, most pointedly — is no
// different: warn in the same wording RefreshBestEffort itself uses for a
// refresh that built fine but failed midway, and skip the refresh, rather
// than failing the command that asked for it.
func refreshIndexBestEffort(ctx context.Context, cctx *CommandContext, reason string) {
	index, err := newCheckoutIndex(cctx)
	if err != nil {
		cctx.Logger.Warn(fmt.Sprintf("Could not refresh the checkout index after %s: %s", reason, err))
		return
	}
	index.RefreshBestEffort(ctx, reason)
}

// RefreshIndex is `index`: refresh the machine-wide checkout index and print
// it, the way src/cli/main.ts's own `index` command does. Unlike down and
// worktree remove, a refresh newCheckoutIndex itself could not even start —
// a symlinked router state directory, say — fails this command outright:
// `index` exists to refresh and show the page, so silently skipping that
// would leave it reporting success while doing nothing.
func RefreshIndex(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	index, err := newCheckoutIndex(cctx)
	if err != nil {
		return err
	}
	snapshot, err := index.Refresh(ctx)
	if err != nil {
		return err
	}
	cctx.Logger.Output(checkoutindex.FormatIndexText(snapshot, index.URL()))
	return nil
}
