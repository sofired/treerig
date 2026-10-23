package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// The destructive verb. Order matters for crash safety: Docker resources go
// first, then the home, then the registration, then the identity marker — so
// an interrupted removal leaves a record that still points at whatever is
// left, and running `remove` again finishes the job. Nothing is deleted until
// every resource has been verified as owned by the target; a single resource
// that fails verification stops the whole thing before it starts.
//
// The ownership-recovery hint below runs router.IndexImage: the digest-pinned
// image the router stack already pulled, so no second image needs fetching
// just to fix an ownership mistake.

// RemoveResult is what a removal did.
type RemoveResult struct {
	Target registry.RegisteredWorktree
	// Resources is everything the worktree owned that is now gone, in inventory
	// order: what stopping it removed and what was reaped.
	Resources    []worktree.OwnedResource
	Home         bool
	Registration bool
	// Marker is set when the worktree's identity marker was removed, so its next `up` mints a fresh identity.
	Marker bool
	// AlreadyGone means another removal finished first; nothing was left for this one.
	AlreadyGone bool
	// NetworkKept means one of the worktree's networks could not be
	// removed (backend.ErrNetworkKept: Compose's "has active endpoints"
	// refusal, tolerated rather than failed). When true, the home directory, registration and identity
	// marker are deliberately left in place too — Home, Registration and
	// Marker all read false — so a later `worktree remove` still finds this
	// worktree and can finish once the network clears.
	NetworkKept bool
}

// Remove is `worktree remove`. A worktree named by UUID, host, the path to its
// directory or slug needs no treerig.toml (ContextWithoutConfig), so it can be
// removed from anywhere; with
// no name it removes the worktree it is run from. A treerig.toml that does not
// parse is warned about and set aside: removing a worktree whose branch broke
// its config is a job this command has to do.
func Remove(ctx context.Context, env *Env, options RemoveOptions) error {
	// Before anything else, the teardown gate (#146): run the pinned,
	// verified teardown binary in place of this build, or refuse. The only
	// way to run this build's own code is --dev-teardown, which relocates it
	// to a standalone copy outside any worktree first (#7), so deleting the
	// worktree below can never delete the program doing the removal.
	// RemoveWithContext, which tests call directly, is deliberately not
	// where this lives: a test's Env has no Executable/Exec wired, so the
	// gate does nothing and the removal runs exactly as it always has.
	if err := teardownGate(ctx, env, options); err != nil {
		return err
	}
	cctx, err := ContextWithoutConfig(env)
	if err != nil {
		return err
	}
	_, err = RemoveWithContext(ctx, cctx, env, options)
	return err
}

// RemoveWithContext removes a worktree's environment; env serves the confirmation prompt.
func RemoveWithContext(ctx context.Context, cctx *CommandContext, env *Env, options RemoveOptions) (*RemoveResult, error) {
	// Where the command runs from, asked once: which repository it is in (if
	// any) decides what no name means, and whether the target is local.
	repositoryID, present, inside, err := cctx.Registry.Identities.Repository(ctx, cctx.Registry.Identities.Root)
	if err != nil {
		// A named target does not depend on where the command runs from: a
		// Git problem here (an unsafe directory, a corrupt .git) only means the
		// target cannot be taken for this repository's own, so the namespace
		// must be recorded, which is the safe side. Naming nothing means "the
		// worktree I am in", and has no answer without Git.
		if options.Identifier == "" {
			return nil, err
		}
		cctx.Logger.Warn(fmt.Sprintf("Could not tell which repository %s belongs to, so %s is treated as another repository's worktree: %s",
			text.Printable(cctx.Cwd), text.Printable(options.Identifier), text.OneLine(err.Error(), 200)))
		repositoryID, present, inside = "", false, false
	}
	scope := repositoryID
	if !present {
		scope = ""
	}
	target, err := resolveRemoveTarget(ctx, cctx, options, inside, scope)
	if err != nil {
		return nil, err
	}
	// What an interrupted removal tells the user to run again. A UUID or a host
	// reads the same from anywhere; a slug may name another worktree from another
	// repository, and a path reads from the directory it is run in, so those are
	// named by UUID instead.
	retry := options
	if options.Identifier != "" && !registry.IsUUID(options.Identifier) &&
		!strings.EqualFold(options.Identifier, layout.WorktreeHost(target.Slug, target.Domain)) {
		retry.Identifier, retry.Domain = target.WorktreeID, ""
	}
	ref := toRef(*target)
	host := layout.WorktreeHost(target.Slug, target.Domain)
	// Refused here, not only in mutate: the inventory below calls Docker and
	// the prompt needs a terminal, and neither should stand between a hook
	// and the reason it cannot remove its own worktree.
	if err := cctx.refuseOwnWorktreeLock(ref.WorktreeID, host); err != nil {
		return nil, err
	}
	paths := layout.WorktreeLayout(cctx.ControlHome, target.RepositoryID, target.WorktreeID)
	local := present && repositoryID == target.RepositoryID
	namespace, err := targetNamespace(cctx, target, local, options)
	if err != nil {
		return nil, err
	}
	runtime := cctx.LabelRuntime(namespace)
	// The shared stack is labelled under its own repository's namespace: the
	// running command's for a worktree of this repository, when its
	// treerig.toml loaded, and the target's recorded one for a worktree of
	// another, which this directory's treerig.toml (if any) says nothing
	// about.
	stackNamespace := namespace
	if local && cctx.Loaded != nil {
		stackNamespace = cctx.stackNamespace()
	}

	// A removal marker this build cannot read (a newer format, or not one it
	// wrote) refuses the removal here, before the inventory and the prompt:
	// nothing is asked or touched on the strength of state it cannot judge.
	if _, err := cctx.Registry.Store(target.RepositoryID).Removing(target.WorktreeID); err != nil {
		return nil, err
	}

	// Everything that will go, verified before anything is touched. A resource
	// that cannot be proven ours fails here, before the prompt.
	planned, err := runtime.Inventory(ctx, ref)
	if err != nil {
		return nil, err
	}
	hasHome, err := anyExists(paths.Home, paths.QuarantinePath)
	if err != nil {
		return nil, err
	}
	if err := confirm(env, DescribePlan(host, planned, hasHome, countBackups(paths.BackupsDir)), host, options.Yes); err != nil {
		return nil, err
	}

	// Whether the target is the worktree this command runs from, which `up`
	// can start again from here; only an interruption's notice reads it.
	here := options.Identifier == ""
	if !here && present {
		if current, err := cctx.Registry.Current(ctx); err == nil && current != nil {
			here = current.WorktreeID == target.WorktreeID
		}
	}
	result := &RemoveResult{Target: *target}
	store := cctx.Registry.Store(target.RepositoryID)
	// Set once the registration is pending, which is when an interruption
	// has left something worth telling the user about.
	marked := false
	err = cctx.mutate(ctx, ref, host, true, func(ctx context.Context) error {
		// The lock was free while the prompt was up: another removal may have
		// finished, or an `up` may have re-labelled the worktree's resources.
		found, err := store.Find(target.WorktreeID)
		if err != nil {
			return err
		}
		if found == nil {
			cctx.Logger.Info(fmt.Sprintf("%s was already removed while '%s worktree remove' was waiting; nothing left to do.", host, cctx.CommandName))
			result.AlreadyGone = true
			return nil
		}
		if found.LabelNamespace != nil && *found.LabelNamespace != namespace {
			return clierr.Newf("%s was re-labelled under %s while '%s worktree remove' was waiting; run it again.", host, *found.LabelNamespace, cctx.CommandName)
		}
		var identity *registry.WorktreeIdentity
		if local {
			if identity, err = targetIdentity(ctx, cctx, target.WorktreeID); err != nil {
				return err
			}
		}
		// Teardown starts here, so the marker goes down before anything is
		// changed: a removal that is interrupted from now on leaves the
		// worktree reading as removing until it is run again, and the
		// registration deletion below takes the marker with it. A marker of a
		// newer format refuses the removal before it touches anything.
		if err := store.MarkRemoving(ctx, target.WorktreeID, target.Slug, cctx.writer()); err != nil {
			return err
		}
		// Unroutes before this worktree's own resources are reaped — see
		// unrouteWorktree's own comment. Runs regardless of which resource
		// kinds current (below) turns out to hold, and works even for a
		// target this process has no treerig.toml for at all.
		if err := unrouteWorktree(cctx, ref); err != nil {
			return err
		}
		// Host routes (#6) go with the worktree, before the record that holds
		// them: `down` keeps them, this does not.
		if err := unrouteHostRoutes(cctx, ref); err != nil {
			return err
		}
		// Shared services are disconnected before anything of this worktree
		// is stopped or reaped, from the networks it owns: an attached
		// shared container would keep a network from being removed.
		if err := disconnectShared(ctx, cctx, ref, stackNamespace, namespace); err != nil {
			return err
		}
		// `down` acts on the project name, so any target can be stopped
		// gracefully before its containers are reaped; skipped when there is
		// nothing to stop. Decided under the lock, so a container that appeared
		// since the prompt is stopped rather than killed.
		current, err := runtime.Inventory(ctx, ref)
		if err != nil {
			return err
		}
		if containsKind(current, "container") {
			if err := withProgress(cctx.Logger, "Stopping "+host, func() error { return runtime.Down(ctx, ref) }); err != nil {
				return err
			}
		}
		if err := withProgress(cctx.Logger, "Removing "+host, func() error {
			// Re-verified after `down`; every resource is checked before any is removed.
			owned, err := runtime.Inventory(ctx, ref)
			if err != nil {
				return err
			}
			// From here on data is destroyed. Should this be interrupted, the
			// next `up` must seed again rather than start on empty volumes.
			if err := store.MarkPending(ctx, target.WorktreeID, target.Slug, cctx.writer()); err != nil {
				return err
			}
			marked = true
			// What `down` took, in case the signal lands before the first reap.
			result.Resources = excludeResources(current, owned)
			// Again, immediately before the first Reap: cheap, idempotent,
			// and it closes a connect that slipped in since the first.
			if err := disconnectShared(ctx, cctx, ref, stackNamespace, namespace); err != nil {
				return err
			}
			var removed, kept []worktree.OwnedResource
			for i, resource := range owned {
				if err := runtime.Reap(ctx, ref, resource); err != nil {
					if errors.Is(err, backend.ErrNetworkKept) {
						kept = append(kept, resource)
						continue
					}
					// What an interruption's notice counts as gone: not the
					// resource that failed, nor those after it.
					result.Resources = excludeResources(mergeResources(current, removed), slices.Concat(kept, owned[i:]))
					return err
				}
				removed = append(removed, resource)
			}
			result.NetworkKept = len(kept) > 0
			// Everything actually gone, in inventory order — what `down`
			// took together with what was reaped. mergeResources unions with
			// current (the pre-down inventory), which still lists a kept
			// resource regardless of whether the loop above actually removed
			// it, so excludeResources drops it again by kind and id: the
			// summary must count only what really left.
			result.Resources = excludeResources(mergeResources(current, removed), kept)
			return nil
		}); err != nil {
			return err
		}
		// A kept network means this worktree is not actually gone: its home,
		// registration and identity marker are left in place, deliberately,
		// so a later `worktree remove` still finds it (store.MarkPending's
		// own record stays findable — nothing above deletes it) and can
		// finish once the network clears. Found in review (round 2 of #55):
		// a first cut removed all three regardless, over a network Compose
		// was still refusing to remove — a permanent leak, invisible to any
		// later `worktree remove`, since nothing was left registered to
		// retry against.
		if result.NetworkKept {
			return nil
		}
		if result.Home, err = removeHome(paths, cctx.CommandName); err != nil {
			return err
		}
		// The containers are gone (Reap above), so no old owner can still
		// hold the worktree's loopback ports: release them before the record
		// itself goes, so a removal interrupted between the two leaves the
		// ports reachable from a record that still names them (#119).
		if err := store.ReleasePorts(ctx, target.WorktreeID, target.Slug, cctx.writer()); err != nil {
			return err
		}
		if result.Registration, err = store.Remove(ctx, target.WorktreeID, target.Slug, cctx.writer()); err != nil {
			return err
		}
		// TODO(#211): a removal from outside the repository (not local) leaves the marker in the directory too.
		if identity != nil {
			if err := cctx.Registry.Identities.RemoveWorktreeMarker(*identity); err != nil {
				return err
			}
			result.Marker = true
		}
		return nil
	})
	if err != nil {
		// After the locks are released, so the notice is the last thing
		// said before the process dies by the signal.
		if notice := interruptNotice(marked, context.Cause(ctx), err, result, host, removeNextSteps(cctx.CommandName, retry, here)); notice != "" {
			cctx.Logger.Info(notice)
		}
		return nil, err
	}
	// Best effort: prints nothing when the refresh succeeds.
	refreshIndexBestEffort(ctx, cctx, "worktree remove")
	switch {
	case result.AlreadyGone:
	case result.NetworkKept:
		cctx.Logger.Warn(fmt.Sprintf(
			"%s: %s, but a network of it is still in use and was left in place, so its home directory, "+
				"registration and identity marker were kept too. Run '%s worktree remove' again once that clears to finish.",
			host, summarize(result), cctx.CommandName))
	default:
		cctx.Logger.Info(fmt.Sprintf("Removed %s: %s.", host, summarize(result)))
		hintSharedDown(ctx, cctx, target.RepositoryID, stackNamespace, local)
	}
	return result, nil
}

// resolveRemoveTarget finds the registration to remove: the current
// worktree's own when options.Identifier is empty (inside says whether the
// command runs from a Git work tree at all), or the worktree the name
// identifies otherwise (registry.Registry.Resolve: a UUID, a host, a path to
// its root, or a slug, which options.Domain narrows). repositoryID is the
// repository the command runs in, "" outside one. It errors rather than
// guessing when neither resolves to a registration. Removal is destructive, so
// a path must name the worktree's root.
func resolveRemoveTarget(ctx context.Context, cctx *CommandContext, options RemoveOptions, inside bool, repositoryID string) (*registry.RegisteredWorktree, error) {
	if options.Identifier == "" {
		if !inside {
			return nil, clierr.Newf("There is no worktree to remove here: %s is not inside a Git worktree. "+
				"Name one with '%s worktree remove <worktree>'.", cctx.Cwd, cctx.CommandName)
		}
		current, err := cctx.Registry.Current(ctx)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, clierr.Newf("This worktree is not registered; nothing to remove. "+
				"Name another with '%s worktree remove <worktree>'.", cctx.CommandName)
		}
		return &current.RegisteredWorktree, nil
	}
	resolved, err := cctx.Registry.Resolve(ctx, options.Identifier, cctx.nameOptions(options.Domain, &repositoryID, false))
	if err != nil {
		return nil, err
	}
	return &resolved.Record, nil
}

// targetIdentity is the target's Git identity among this repository's live
// worktrees; only meaningful for a local target.
func targetIdentity(ctx context.Context, cctx *CommandContext, worktreeID string) (*registry.WorktreeIdentity, error) {
	identities, err := cctx.Registry.Identities.ActiveIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, identity := range identities {
		if identity.WorktreeID == worktreeID {
			found := identity
			return &found, nil
		}
	}
	return nil, nil
}

// targetNamespace is which labels the target's resources carry: the
// registration (deleted last, so it survives an interrupted removal); then
// the index card, wherever the home currently is; then, for a worktree of
// this repository whose treerig.toml loaded, that config. Never the resources
// about to be deleted: inventorying under the wrong namespace finds nothing
// and would delete the registration that points at the data.
// --label-namespace is for a worktree none of those describe, and never an
// override of one they do.
func targetNamespace(cctx *CommandContext, target *registry.RegisteredWorktree, local bool, options RemoveOptions) (string, error) {
	flag := options.LabelNamespace
	if flag != "" && !brand.LabelNamespacePattern.MatchString(flag) {
		return "", clierr.Newf("--label-namespace must look like a reverse domain, e.g. dev.example, not \"%s\".", flag)
	}
	host := layout.WorktreeHost(target.Slug, target.Domain)
	recorded := card.RecordedLabelNamespace(cctx.ControlHome, card.Registration{
		RepositoryID: target.RepositoryID, WorktreeID: target.WorktreeID, LabelNamespace: target.LabelNamespace,
	})
	if recorded != nil {
		if flag != "" && flag != *recorded {
			return "", clierr.Newf("%s records its resources as labelled under %s, not %s. "+
				"Drop --label-namespace, or pass the recorded one.", host, *recorded, flag)
		}
		return *recorded, nil
	}
	if flag != "" {
		return flag, nil
	}
	if local && cctx.Loaded != nil {
		return cctx.Loaded.Config.LabelNamespace(), nil
	}
	// Nothing records it. Where the user is decides what they can do about it:
	// a worktree of this repository whose treerig.toml was set aside needs the
	// file fixed; any other needs the command run from inside its repository,
	// naming the target, since a bare 'worktree remove' there would remove the
	// worktree they are in.
	// TODO(#211): --label-namespace goes with the flag; this then asks for nothing but the repository.
	where, remedy := "from here:", fmt.Sprintf("Run '%s worktree remove %s' from inside the repository that created it", cctx.CommandName, target.Slug)
	switch {
	case local:
		where, remedy = "from this directory: its "+brand.ConfigFile+" could not be used to find out, and", "Fix "+brand.ConfigFile
	case cctx.Loaded != nil:
		where = "from this repository: it belongs to a different one, and"
	}
	return "", clierr.Newf("Cannot remove %s %s nothing on disk records which Docker labels its resources carry. %s, or pass "+
		"--label-namespace (%s unless that project set project.label-namespace).", host, where, remedy, brand.LabelNamespace)
}

// removeHome is rename-then-delete. The rename is atomic, so a crash leaves
// either the home or a .deleting- directory — never a half-deleted home that
// `up` might adopt as complete. A leftover quarantine from an earlier attempt
// is finished first.
func removeHome(paths layout.Worktree, commandName string) (bool, error) {
	removed := false
	if exists, err := files.PathExists(paths.QuarantinePath); err != nil {
		return false, err
	} else if exists {
		if err := deleteQuarantine(paths, commandName); err != nil {
			return false, err
		}
		removed = true
	}
	if exists, err := files.PathExists(paths.Home); err != nil {
		return false, err
	} else if exists {
		if err := files.AssertSafeDirectoryPath(paths.ControlHome, paths.Home); err != nil {
			return false, err
		}
		if err := os.Rename(paths.Home, paths.QuarantinePath); err != nil {
			return false, err
		}
		if err := deleteQuarantine(paths, commandName); err != nil {
			return false, err
		}
		removed = true
	}
	return removed, nil
}

// deleteQuarantine explains a container that ran as another uid and left
// files the user cannot delete (a database's data directory, typically), and
// how to get out, rather than reporting a generic interrupted removal forever.
// The recovery runs the digest-pinned image the router stack already pulled,
// as real root, on the quarantine directory alone.
func deleteQuarantine(paths layout.Worktree, commandName string) error {
	err := files.RemoveWithin(paths.ControlHome, paths.QuarantinePath)
	if err == nil || !(errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM)) {
		return err
	}
	where := ""
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		where = " (first refusal: " + pathError.Path + ")"
	}
	// The container runs as root, so the owner is spelled out as the user's
	// own uid:gid — a `$(id -u)` evaluated inside the container would say 0.
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	return clierr.Newf("Could not delete %s%s. Some of its files are most likely owned "+
		"by another user — written by a container running as a different uid. Take ownership back, "+
		"for example with\n\n"+
		"  docker run --rm --network none -v %s %s "+
		"sh -c \"chown -R %s /d && chmod -R u+rwX /d\"\n\n"+
		"then run '%s worktree remove' again to finish.",
		paths.QuarantinePath, where, text.ShellQuote(paths.QuarantinePath+":/d"), router.IndexImage, owner, commandName)
}

// anyExists reports whether any of paths exists on disk, stopping at the
// first one found (or the first stat error).
func anyExists(paths ...string) (bool, error) {
	for _, path := range paths {
		exists, err := files.PathExists(path)
		if err != nil || exists {
			return exists, err
		}
	}
	return false, nil
}

// containsKind reports whether resources includes one of the given kind.
func containsKind(resources []worktree.OwnedResource, kind string) bool {
	for _, resource := range resources {
		if resource.Kind == kind {
			return true
		}
	}
	return false
}

// mergeResources is current then owned, keyed by kind and id, keeping each
// key's first position: owned is normally a subset of current, and anything
// that appeared between the two inventories and was reaped is appended.
func mergeResources(current, owned []worktree.OwnedResource) []worktree.OwnedResource {
	index := map[string]int{}
	var merged []worktree.OwnedResource
	for _, resource := range append(append([]worktree.OwnedResource(nil), current...), owned...) {
		key := resource.Kind + ":" + resource.ID
		if i, ok := index[key]; ok {
			merged[i] = resource
			continue
		}
		index[key] = len(merged)
		merged = append(merged, resource)
	}
	return merged
}

// excludeResources is resources with every entry of exclude (matched by
// kind and id) removed. mergeResources' own union with the pre-removal
// inventory (current) lists a resource whether or not it was actually
// removed later — accurate when nothing is ever left behind, which is no
// longer always true now that Reap can report ErrNetworkKept — so
// RemoveWithContext calls this to drop a kept resource back out before it
// is counted as gone.
func excludeResources(resources, exclude []worktree.OwnedResource) []worktree.OwnedResource {
	if len(exclude) == 0 {
		return resources
	}
	skip := make(map[string]bool, len(exclude))
	for _, resource := range exclude {
		skip[resource.Kind+":"+resource.ID] = true
	}
	var kept []worktree.OwnedResource
	for _, resource := range resources {
		if !skip[resource.Kind+":"+resource.ID] {
			kept = append(kept, resource)
		}
	}
	return kept
}

// countResources renders resources as "N kind, M kind, …" grouped by kind in
// first-seen order, or "no Docker resources" when there are none — the
// phrase both the confirmation plan and the removal summary use.
func countResources(resources []worktree.OwnedResource) string {
	var kinds []string
	counts := map[string]int{}
	for _, resource := range resources {
		if counts[resource.Kind] == 0 {
			kinds = append(kinds, resource.Kind)
		}
		counts[resource.Kind]++
	}
	if len(kinds) == 0 {
		return "no Docker resources"
	}
	parts := make([]string, len(kinds))
	for i, kind := range kinds {
		plural := "s"
		if counts[kind] == 1 {
			plural = ""
		}
		parts[i] = fmt.Sprintf("%d %s%s", counts[kind], kind, plural)
	}
	return strings.Join(parts, ", ")
}

// countBackups is how many bundles `backup` has left under dir, the
// worktree's default backups directory. A directory that is missing or cannot
// be read counts as none: the plan is a courtesy, and must never stand
// between the user and a removal.
func countBackups(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// DescribePlan is what the confirmation screen lists. The host is filtered
// here, where it is borrowed: a hostname that carried a line break would
// otherwise write its own line into this list.
func DescribePlan(host string, planned []worktree.OwnedResource, hasHome bool, backups int) string {
	home := "  (no home directory)"
	if hasHome {
		home = "  its home directory under the control home"
		if backups > 0 {
			// `backup` without a directory writes here, and the bundles go
			// with the home: say so at the point of loss.
			home += fmt.Sprintf(", including %d backup bundle(s) in %s/", backups, layout.BackupsDirName)
		}
	}
	return strings.Join([]string{
		fmt.Sprintf("This will permanently remove %s:", text.Printable(host)),
		"  " + countResources(planned),
		home,
		"  its registration",
	}, "\n")
}

// removeNextSteps is the two commands an interrupted removal points to:
// the one that finishes it, naming the target the way it was named to this
// command, and the one that starts the worktree again, which only runs from
// the target's own worktree (empty otherwise).
func removeNextSteps(commandName string, options RemoveOptions, here bool) nextSteps {
	// The values are echoed as typed, unquoted: slugs, UUIDs, domains and label
	// namespaces are plain tokens, and the whole notice goes through
	// text.Printable before it is shown.
	finish := commandName + " worktree remove"
	if options.Identifier != "" {
		finish += " " + options.Identifier
		if options.Domain != "" {
			finish += " --domain " + options.Domain
		}
	}
	// A namespace given on the command line is one the registration may not
	// record, so finishing needs it again.
	if options.LabelNamespace != "" {
		finish += " --label-namespace " + options.LabelNamespace
	}
	steps := nextSteps{finish: finish}
	if here {
		steps.restart = commandName + " up"
	}
	return steps
}

// nextSteps is what an interrupted removal tells the user they can run.
type nextSteps struct {
	// finish is the command that completes the removal.
	finish string
	// restart is the command that starts the worktree again, or empty when
	// that cannot be run from here.
	restart string
}

// interruptNotice is what an interrupted removal says once the worktree is
// pending, or "" when it has nothing to add. It speaks only when all of these
// hold: the registration was marked pending; it still exists; a signal is why
// the context ended (cause); and err is that interruption alone. An error
// that carries more (a lock that could not be released, say) is reported on
// its own and recovered through the lock, so a pending notice would mislead.
//
// The resources counted are a lower bound: the one being removed when the
// signal came, and those after it, are not counted even if the daemon
// finished the one in flight. The line is filtered as a whole, since it
// carries the host.
func interruptNotice(marked bool, cause, err error, result *RemoveResult, host string, next nextSteps) string {
	var interrupted, plain *clierr.Cancelled
	if !marked || result.Registration || !errors.As(cause, &interrupted) || !errors.As(err, &plain) {
		return ""
	}
	var gone []string
	if len(result.Resources) > 0 {
		gone = append(gone, countResources(result.Resources))
	}
	if result.Home {
		gone = append(gone, "home directory")
	}
	removed := "Removed so far: nothing confirmed yet"
	if len(gone) > 0 {
		removed = "Removed so far (at least): " + strings.Join(gone, ", ")
	}
	steps := fmt.Sprintf("'%s' finishes the removal", next.finish)
	if next.restart != "" {
		steps += fmt.Sprintf(", '%s' calls it off and runs the seed hook again", next.restart)
	}
	return text.Printable(fmt.Sprintf("Interrupted (%s) while removing %s. %s. It is still registered and now reads 'removing': %s.",
		clierr.SignalName(interrupted.Signal), host, removed, steps))
}

// summarize renders what a completed removal actually took away, for the
// closing "Removed <host>: …" line: resource counts plus which of the home
// directory, registration and identity marker were present and removed.
func summarize(result *RemoveResult) string {
	parts := []string{countResources(result.Resources)}
	if result.Home {
		parts = append(parts, "home directory")
	}
	if result.Registration {
		parts = append(parts, "registration")
	}
	if result.Marker {
		parts = append(parts, "identity marker")
	}
	return strings.Join(parts, ", ")
}
