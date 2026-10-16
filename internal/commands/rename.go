package commands

// The `worktree rename` verb (#11): change the slug a worktree is registered
// under, and with it its hostname.
//
// What derives from the slug, and what a rename does about each:
//
//   - the registry record: rewritten, last, under the registry lock, which is
//     also where the slug is proven free in the record's domain;
//   - the route files (<slug>.<domain> in their Host rules): a service's
//     removed, a host route's rewritten from the renamed record, both before
//     the record, see moveRoutes;
//   - the index card (slug, host and service URLs): rewritten before the
//     record, see moveIndexCard;
//   - the checkout index page: refreshed afterwards, best effort;
//   - the hostname the worktree's containers were created with
//     (TREERIG_SLUG and TREERIG_HOST reach Compose, so a container carries them):
//     this is why a rename refuses while any container of the worktree
//     exists, running or stopped, and asks for `down` first.
//
// Everything else is keyed by the worktree's IDs, not its slug, and is
// untouched: the Compose project name (treerig-<hash of the two IDs>) and with
// it every container, network and volume name, the home directory, the lock
// files, the pinned loopback ports (kept in the record), and a shared
// service's connection (made by network ID, under the service's own name).
// So treerig renames and deletes no volume. A Compose file that bakes
// ${TREERIG_SLUG} or ${TREERIG_HOST} into a volume name would name a new volume
// after the rename; treerig cannot see inside the file, and the old volume is
// left like any other, owned by the worktree's labels, until `worktree
// remove` removes it.

import (
	"context"
	"fmt"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/text"
)

// RenameOptions is `worktree rename`'s input.
type RenameOptions struct {
	// Slug is the new slug.
	Slug string
}

// RenameResult is what a rename did.
type RenameResult struct {
	Previous registry.RegisteredWorktree
	Current  registry.RegisteredWorktree
	// Unchanged means the worktree already held the slug asked for.
	Unchanged bool
	// Card is whether the index card was rewritten.
	Card bool
}

// Rename is `worktree rename`.
func Rename(ctx context.Context, env *Env, options RenameOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = RenameWithContext(ctx, cctx, options)
	return err
}

// RenameWithContext renames the current worktree. It runs under the
// worktree's operation and development-session locks, so it cannot overlap an
// `up`, a `down` or an attached `dev`, with the registry lock nested inside
// for the slug check and the record write (Store.Rename).
//
// The order is crash-safe in that the record changes last: derived state is
// dealt with first, each step idempotent and independent of the slug an
// earlier attempt was given, so an interrupted rename leaves the old slug
// registered and running it again, with any slug, completes it. The record is
// deliberately not marked pending first, as a removal does: nothing is
// destroyed, and a pending mark would make the next `up` seed again.
func RenameWithContext(ctx context.Context, cctx *CommandContext, options RenameOptions) (*RenameResult, error) {
	if err := registry.ValidateSlug(options.Slug); err != nil {
		return nil, err
	}
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return nil, err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	if ref.Slug == options.Slug {
		cctx.Logger.Info(fmt.Sprintf("This worktree is already registered as %s (%s); nothing to rename.", options.Slug, host))
		return &RenameResult{Previous: registration.RegisteredWorktree, Current: registration.RegisteredWorktree, Unchanged: true}, nil
	}
	// The record's namespace, then the card's, then this config's: the one
	// the worktree's containers are actually labelled under.
	namespace, err := targetNamespace(cctx, &registration.RegisteredWorktree, true, RemoveOptions{})
	if err != nil {
		return nil, err
	}
	runtime := cctx.Runtime(namespace)
	store := cctx.Registry.Store(ref.RepositoryID)

	result := &RenameResult{Previous: registration.RegisteredWorktree}
	err = cctx.mutate(ctx, ref, host, true, func(ctx context.Context) error {
		// Proven by labels (Inventory verifies each resource), decided under
		// the lock: a container started since the registration was read counts.
		owned, err := runtime.Inventory(ctx, ref)
		if err != nil {
			return err
		}
		containers := 0
		for _, resource := range owned {
			if resource.Kind == "container" {
				containers++
			}
		}
		if containers > 0 {
			return clierr.Newf(
				"%s still has %s, whose environment carries the old hostname. Run '%s down' first, then rename it.",
				host, countNoun(containers, "container"), cctx.CommandName)
		}
		renamed, err := store.Rename(ctx, ref.WorktreeID, ref.Slug, options.Slug, cctx.writer(),
			func(previous, renamed registry.RegisteredWorktree) error {
				if err := moveRoutes(cctx, renamed); err != nil {
					return err
				}
				var err error
				result.Card, err = moveIndexCard(cctx, previous, renamed)
				return err
			},
			func(holder registry.RegisteredWorktree) string { return lastKnownRoot(cctx, holder) })
		if err != nil {
			return err
		}
		result.Current = renamed
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Best effort: a stale index is an inconvenience, not a failed rename.
	refreshIndexBestEffort(ctx, cctx, "worktree rename")
	newHost := layout.WorktreeHost(result.Current.Slug, result.Current.Domain)
	cctx.Logger.Info(fmt.Sprintf("Renamed %s to %s. Run '%s up' to start it there.", host, newHost, cctx.CommandName))
	return result, nil
}

// moveRoutes is the one place a rename deals with every route file a
// worktree owns, and the seam for any other kind of file that carries the
// hostname. Two kinds do today.
//
// The route files of Compose services are removed: they exist only while
// containers do (`down` removes them), a rename refuses while containers
// exist, so these are crash leftovers, dead routes to a stopped worktree, and
// `up` writes them afresh at the new hostname.
//
// The route files of host routes (#6) outlive `down`, so they are rewritten:
// each recorded route's file is written from the renamed record (the new slug
// and domain and the route's own definition), never by replacing the old
// hostname in the file, and before the record changes. A route whose file is
// missing gets one, which is the heal `route add` offers. Both are what keep
// a retried rename safe whatever slug it is retried with: a file left by an
// attempt that crashed after this step is overwritten from the record, and
// none is left at a hostname no record owns. A file the record has no route
// for is not touched here; the router's repair removes it.
//
// Shared services' route files are keyed by repository and unaffected by a
// worktree's slug.
func moveRoutes(cctx *CommandContext, renamed registry.RegisteredWorktree) error {
	if err := router.RemoveWorktreeFragments(cctx.RouterState, renamed.RepositoryID, renamed.WorktreeID); err != nil {
		return clierr.Wrap(fmt.Sprintf("Could not remove this worktree's route files before renaming it to %s", layout.WorktreeHost(renamed.Slug, renamed.Domain)), err)
	}
	for _, route := range renamed.HostRoutes {
		if err := router.WriteHostFragment(cctx.RouterState, renamed.RepositoryID, renamed.WorktreeID, route.Name, hostRouteDocument(renamed, route)); err != nil {
			return clierr.Wrap(fmt.Sprintf("Could not rewrite the route file of host route %q before renaming it to %s", route.Name, layout.WorktreeHost(renamed.Slug, renamed.Domain)), err)
		}
	}
	return nil
}

// moveIndexCard points the worktree's index card at its new hostname, so the
// checkout index keeps listing its services. It moves from whatever host the
// card itself names, so a card a failed attempt already moved is moved on.
func moveIndexCard(cctx *CommandContext, previous, renamed registry.RegisteredWorktree) (bool, error) {
	moved, err := card.Rehost(cctx.ControlHome, renamed.RepositoryID, renamed.WorktreeID, renamed.Slug, layout.WorktreeHost(renamed.Slug, renamed.Domain))
	if err != nil {
		return false, clierr.Wrap(fmt.Sprintf("Could not update the index card for %s", layout.WorktreeHost(previous.Slug, previous.Domain)), err)
	}
	return moved, nil
}

// lastKnownRoot is where holder's checkout last was, from its index card,
// printable, or "" when it has none; the way `worktree list` names a worktree.
func lastKnownRoot(cctx *CommandContext, holder registry.RegisteredWorktree) string {
	seen, err := card.ReadIndexCard(cctx.ControlHome, holder.RepositoryID, holder.WorktreeID)
	if err != nil || seen == nil || seen.Root == "" {
		return ""
	}
	return text.Printable(seen.Root)
}

// countNoun is "1 container", "2 containers".
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
