package commands

// The overview page's service: newOverview and overviewReader build the real
// overview.Service that every mutating verb which must refresh the page
// afterward (`down`, `worktree remove`; `up` takes an Index the same way it
// takes a Router) and `list` read from.

import (
	"context"
	"fmt"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/overview"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/router"
)

// routerLockScope is the one place in this build that builds the shared
// router's lock scope, always from an already-resolved layout.RouterLayout
// (a caller's own state, itself only ever reached through resolveRouterState
// in home.go, which applies files.AssertSafeHome to TREERIG_HOME first) —
// never re-derived from a raw Getenv, so there is no second, unchecked path
// to the router's lock target (#80). Every caller that takes the router's
// lock (routerHold, unlock.go's router branch, and doctor.go's own report of
// it) goes through this one function: a caller that already has the
// router's own, resolved ProjectName (RouterUp, via
// newRouterService/router.New) and one that has no router.Router to ask
// (newOverview, below, called from `down` and `worktree remove`;
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

// newOverview builds the real overview Service for cctx's own
// worktree: the machine-wide registry and Git identities it already reads
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
// symlinked state directory" test is exactly this check; newOverview
// used to skip it entirely.
func newOverview(cctx *CommandContext, use overviewUse) (*overview.Service, error) {
	if err := router.SecureState(cctx.RouterState); err != nil {
		return nil, err
	}
	return overviewReader(cctx, use), nil
}

// overviewUse is what an overview.Service is built for, which decides what it
// says about what it could not ask.
type overviewUse int

const (
	// refreshesPage is a refresh after a lifecycle verb: it warns when Docker
	// did not answer, and says nothing of repositories Git cannot be asked
	// about, which would be reported after every command if one stays offline.
	refreshesPage overviewUse = iota
	// listsWorktrees is `list`: no warning that Docker did not answer, since the
	// command ends in the cause itself, and a warning naming the repositories
	// Git could not be asked about.
	listsWorktrees
)

// overviewDockerBudget bounds the one `docker ps` per label namespace an
// overview makes, and Git for everything it asks of it together: a listing, and
// the refresh a lifecycle verb ends with, are read, not waited for, so a daemon
// or a mount that is hung is reported rather than waited out.
const overviewDockerBudget = 5 * time.Second

// overviewReader is the overview Service without newOverview's first step:
// the router's state directory is neither secured nor created, so a Service
// that only takes snapshots (`list`) writes nothing and takes no lock. Its
// Refresh would write the page; callers that refresh go through newOverview.
func overviewReader(cctx *CommandContext, use overviewUse) *overview.Service {
	return overview.NewService(overview.Options{
		ControlHome:        cctx.ControlHome,
		RouterPort:         cctx.RouterPort,
		IndexFile:          cctx.RouterState.IndexFile,
		Lock:               routerLockScope(cctx.RouterState, ""),
		Registry:           cctx.Registry,
		Identities:         cctx.Registry.Identities,
		IdentityRoot:       cctx.Registry.Identities.Root,
		Runner:             cctx.Runner,
		Logger:             cctx.Logger,
		Invocation:         cctx.Invocation,
		InventoryTimeout:   overviewDockerBudget,
		GitTimeout:         overviewDockerBudget,
		Environ:            cctx.Environ,
		QuietDocker:        use == listsWorktrees,
		WarnOfRepositories: use == listsWorktrees,
	})
}

// refreshOverviewBestEffort is what down and worktree remove call instead of
// newOverview directly: a refresh is best-effort everywhere else in
// this build (RefreshBestEffort's own doc comment), and newOverview
// failing — a symlinked router state directory, most pointedly — is no
// different: warn in the same wording RefreshBestEffort itself uses for a
// refresh that built fine but failed midway, and skip the refresh, rather
// than failing the command that asked for it.
func refreshOverviewBestEffort(ctx context.Context, cctx *CommandContext, reason string) {
	index, err := newOverview(cctx, refreshesPage)
	if err != nil {
		cctx.Logger.Warn(fmt.Sprintf("Could not refresh the overview after %s: %s", reason, err))
		return
	}
	index.RefreshBestEffort(ctx, reason)
}
