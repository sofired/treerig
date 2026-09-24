package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
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
}

// Remove is `worktree remove`.
func Remove(ctx context.Context, env *Env, options RemoveOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = RemoveWithContext(ctx, cctx, env, options)
	return err
}

// RemoveWithContext removes a worktree's environment; env serves the confirmation prompt.
func RemoveWithContext(ctx context.Context, cctx *CommandContext, env *Env, options RemoveOptions) (*RemoveResult, error) {
	target, err := resolveRemoveTarget(ctx, cctx, options)
	if err != nil {
		return nil, err
	}
	ref := toRef(*target)
	host := layout.WorktreeHost(target.Slug, target.Domain)
	paths := layout.WorktreeLayout(cctx.ControlHome, target.RepositoryID, target.WorktreeID)
	repositoryID, present, err := cctx.Registry.Identities.RepositoryID(ctx, cctx.Registry.Identities.Root)
	if err != nil {
		return nil, err
	}
	local := present && repositoryID == target.RepositoryID
	namespace, err := targetNamespace(cctx, target, local, options)
	if err != nil {
		return nil, err
	}
	runtime := cctx.Runtime(namespace)

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
	if err := confirm(env, DescribePlan(host, planned, hasHome), host, options.Yes); err != nil {
		return nil, err
	}

	result := &RemoveResult{Target: *target}
	store := cctx.Registry.Store(target.RepositoryID)
	err = cctx.mutate(ctx, ref, true, func(ctx context.Context) error {
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
			for _, resource := range owned {
				if err := runtime.Reap(ctx, ref, resource); err != nil {
					return err
				}
			}
			// Everything now gone, in inventory order — what `down` took
			// together with what was reaped — so the summary lists kinds the
			// way the plan did.
			result.Resources = mergeResources(current, owned)
			return nil
		}); err != nil {
			return err
		}
		if result.Home, err = removeHome(paths, cctx.CommandName); err != nil {
			return err
		}
		if result.Registration, err = store.Remove(ctx, target.WorktreeID, target.Slug, cctx.writer()); err != nil {
			return err
		}
		if identity != nil {
			if err := cctx.Registry.Identities.RemoveWorktreeMarker(*identity); err != nil {
				return err
			}
			result.Marker = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// TODO(#41): refresh the checkout index here, best effort, as the
	// TypeScript build does; it prints nothing when that refresh succeeds.
	if !result.AlreadyGone {
		cctx.Logger.Info(fmt.Sprintf("Removed %s: %s.", host, summarize(result)))
	}
	return result, nil
}

func resolveRemoveTarget(ctx context.Context, cctx *CommandContext, options RemoveOptions) (*registry.RegisteredWorktree, error) {
	if options.Identifier == "" {
		current, err := cctx.Registry.Current(ctx)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, clierr.Newf("This worktree is not registered; nothing to remove. "+
				"Name another with '%s worktree remove <slug|uuid>'.", cctx.CommandName)
		}
		return &current.RegisteredWorktree, nil
	}
	found, err := cctx.Registry.FindGlobal(ctx, options.Identifier, options.Domain)
	if err != nil {
		return nil, err
	}
	if found == nil {
		where := ""
		if options.Domain != "" {
			where = " under " + options.Domain
		}
		return nil, clierr.Newf("No worktree is registered as %s%s.", options.Identifier, where)
	}
	return found, nil
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
// this repository, this config. --label-namespace is for a worktree none of
// those describe — never an override of one they do, because inventorying
// under the wrong namespace finds nothing and would delete the registration
// that points at the data.
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
	if local {
		return cctx.Loaded.Config.LabelNamespace(), nil
	}
	return "", clierr.Newf("Cannot remove %s from this repository: it belongs to a different one, "+
		"and nothing on disk records which Docker labels its resources carry. Run "+
		"'%s worktree remove' from a checkout of the repository that created it, or pass "+
		"--label-namespace (%s unless that project set project.label-namespace).", host, cctx.CommandName, brand.LabelNamespace)
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

func anyExists(paths ...string) (bool, error) {
	for _, path := range paths {
		exists, err := files.PathExists(path)
		if err != nil || exists {
			return exists, err
		}
	}
	return false, nil
}

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

// DescribePlan is what the confirmation screen lists. The host is filtered
// here, where it is borrowed: a hostname that carried a line break would
// otherwise write its own line into this list.
func DescribePlan(host string, planned []worktree.OwnedResource, hasHome bool) string {
	home := "  (no home directory)"
	if hasHome {
		home = "  its home directory under the control home"
	}
	return strings.Join([]string{
		fmt.Sprintf("This will permanently remove %s:", text.Printable(host)),
		"  " + countResources(planned),
		home,
		"  its registration",
	}, "\n")
}

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
