package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry"
)

// The one way a dead holder's worktree lock is cleared, ported from
// src/commands/unlock.ts and src/lock-recovery.ts onto the flock lock layer
// (see internal/lock's package comment). It never guesses: lock.Clear only
// ever marks a holder's record released once flock(2) itself proves nobody
// holds the lock and its record's identity checks prove that holder gone —
// or, with --holder-unverifiable, once the person running the command has
// said so. It takes no lock of its own: both of a worktree's locks are
// probed read-only first (lock.ProbeHeld), so an active one is always
// refused before anything is cleared, never partway through.

// UnlockOptions are `worktree unlock`'s argument and flags.
type UnlockOptions struct {
	// Identifier is a slug or worktree UUID; this checkout when empty.
	Identifier string
	Domain     string
	// AllowUnverifiable accepts a holder this machine cannot check (another
	// machine or PID namespace, or no record) — --holder-unverifiable.
	AllowUnverifiable bool
}

// ClearedLock is one lock UnlockWithContext cleared.
type ClearedLock struct {
	// Kind is "operation" or "development", matching lock.WorktreeScopes.
	Kind        string
	Description string
	Target      string
}

// UnlockResult is what `worktree unlock` did.
type UnlockResult struct {
	// Name is the worktree, as its locks name it: its slug, or its root
	// before it has one.
	Name    string
	Cleared []ClearedLock
}

// uuidPattern matches a worktree UUID, for the extra clause a refusal adds
// when the identifier looks like one.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// unlockTarget is the worktree to unlock, named the way its locks are.
type unlockTarget struct {
	repositoryID string
	worktreeID   string
	name         string
	// host is "" for a worktree not (or no longer) registered.
	host string
}

// Unlock is `worktree unlock`.
func Unlock(ctx context.Context, env *Env, options UnlockOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = UnlockWithContext(ctx, cctx, options)
	return err
}

// UnlockWithContext resolves the target worktree, refuses while either of
// its locks is actively held, does nothing when neither is held, and
// otherwise clears every stale one together — judging both, then committing
// both, so a refusal on one never leaves the other already cleared (see
// lock.Clear) — judging each holder gone (or accepting it unverified, with
// options.AllowUnverifiable) before marking it released.
func UnlockWithContext(ctx context.Context, cctx *CommandContext, options UnlockOptions) (*UnlockResult, error) {
	target, err := resolveUnlockTarget(ctx, cctx, options)
	if err != nil {
		return nil, err
	}
	label := target.host
	if label == "" {
		label = target.name
	}

	markers := procid.Markers{Project: layout.ComposeProjectName(target.repositoryID, target.worktreeID), WorktreeID: target.worktreeID}
	operationScope, developmentScope := lock.WorktreeScopes(cctx.ControlHome, target.repositoryID, target.worktreeID, target.name, markers)
	// Development, then operation: the order every waiter and every earlier
	// report (doctor's Locks section, a mutation's own refusal) already names them in.
	kinds := map[lock.Scope]string{developmentScope: "development", operationScope: "operation"}
	scopes := []lock.Scope{developmentScope, operationScope}

	table := procid.Platform()
	for _, scope := range scopes {
		held, refusal, err := lock.ProbeHeld(ctx, scope, table)
		if err != nil {
			return nil, err
		}
		if held {
			return nil, refusal
		}
	}

	anyStale := false
	for _, scope := range scopes {
		stale, err := scopeNeedsClearing(scope)
		if err != nil {
			return nil, err
		}
		if stale {
			anyStale = true
			break
		}
	}
	if !anyStale {
		cctx.Logger.Info(fmt.Sprintf("Nothing to unlock: no lock is held for %s.", label))
		return &UnlockResult{Name: target.name}, nil
	}
	// This is the ~15 s TypeScript build's own "Checking..." notice used to
	// promise (src/commands/unlock.ts): that clause named the second look its
	// own heartbeat design always took. flock(2) proves a lock free the
	// instant it is probed, so there is no such wait to promise here — see
	// divergence ledger row 18.
	cctx.Logger.Info(fmt.Sprintf("Checking that the holder of %s's lock, and every command it started on this machine, is gone.", label))

	if env, ok := cctx.environWorktreeID(); ok && env == target.worktreeID {
		return nil, clierr.New(
			"This command is running inside the worktree's own environment (BERTH_WORKTREE_ID is set, as in its hooks), " +
				"so it cannot tell its own processes from the holder's. Run it from an ordinary shell.")
	}

	cleared, clearErr := lock.Clear(ctx, scopes, lock.ClearOptions{
		Table: table, AllowUnverifiable: options.AllowUnverifiable, SuggestUnverifiableFlag: true,
	})

	// Built from cleared.Scopes whether or not lock.Clear itself failed:
	// its own doc comment promises every ScopeResult decided so far, even a
	// commit that failed partway through a batch it had already judged
	// clean, rather than discarding what was actually cleared.
	result := &UnlockResult{Name: target.name}
	var verified, unverified []string
	for _, sr := range cleared.Scopes {
		if !sr.Cleared {
			continue
		}
		result.Cleared = append(result.Cleared, ClearedLock{Kind: kinds[sr.Scope], Description: sr.Scope.Description, Target: sr.Scope.Target})
		for _, reason := range sr.Verified {
			verified = append(verified, fmt.Sprintf("%s: %s", sr.Scope.Description, reason))
		}
		for _, reason := range sr.Unverified {
			unverified = append(unverified, fmt.Sprintf("%s: %s", sr.Scope.Description, reason))
		}
	}

	if clearErr != nil {
		return result, wrapPartialClearFailure(result.Cleared, clearErr)
	}

	if len(result.Cleared) == 0 {
		// The pre-check above found something stale, but it was released —
		// by a race — before Clear reached it.
		cctx.Logger.Info(fmt.Sprintf("Nothing to unlock: the lock on %s was released while it was being checked.", label))
		return result, nil
	}
	// The one line src/lock-recovery.ts's verify() always adds last, once it
	// established no command this worktree started is still running.
	verified = append(verified, cleared.Proof)
	for _, line := range verified {
		cctx.Logger.Info(fmt.Sprintf("Verified: %s.", line))
	}
	for _, line := range unverified {
		cctx.Logger.Warn(fmt.Sprintf("Not verified, on your word (--holder-unverifiable): %s.", line))
	}
	descriptions := make([]string, len(result.Cleared))
	for i, c := range result.Cleared {
		descriptions[i] = c.Description + " lock"
	}
	cctx.Logger.Info(fmt.Sprintf("Unlocked %s: cleared the %s.", label, strings.Join(descriptions, " and the ")))
	return result, nil
}

// wrapPartialClearFailure prefaces err with what a failed lock.Clear had
// already cleared, when it had cleared anything: a commit that fails
// partway through a batch phase 1 already judged clean entirely — after an
// earlier scope in the same batch already committed — must not read as
// though nothing happened. The scope Clear was committing when it failed is
// left exactly as an ordinary stale lock: Clear's own deferred close (see its
// doc comment) frees its flock on the way out regardless, so a later
// `worktree unlock`, or an ordinary Acquire, finds it free and simply
// recovers the still-unreleased record the way it would any other stale one.
// err itself becomes the cause (see cli.go's report(), which prints it once
// on its own "caused by" line), so its text is not repeated in the message
// here. TypeScript's own clearStaleWorktreeLocks has nothing to compare this
// wording against: the same kind of mid-batch failure simply propagates
// there, uncredited (src/operation-lock.ts:826-840), so this note is Go's own
// wording (divergence ledger row 20). err is returned unchanged when cleared
// is empty: lock.Clear never got as far as committing anything, so there is
// nothing to preface.
func wrapPartialClearFailure(cleared []ClearedLock, err error) error {
	if len(cleared) == 0 {
		return err
	}
	descriptions := make([]string, len(cleared))
	for i, c := range cleared {
		descriptions[i] = c.Description + " lock"
	}
	return clierr.Wrap(fmt.Sprintf(
		"Already cleared the %s before this failed. Run this again to clear the rest.",
		strings.Join(descriptions, " and the ")), err)
}

// environWorktreeID is BERTH_WORKTREE_ID from cctx's own process environment.
func (c *CommandContext) environWorktreeID() (string, bool) {
	for _, entry := range c.Environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == "BERTH_WORKTREE_ID" {
			return value, true
		}
	}
	return "", false
}

// resolveUnlockTarget is the worktree to unlock: one named by slug or UUID
// anywhere on this machine — by UUID even if it never registered — or else
// this checkout, by its identity markers.
func resolveUnlockTarget(ctx context.Context, cctx *CommandContext, options UnlockOptions) (*unlockTarget, error) {
	if options.Identifier != "" {
		found, err := cctx.Registry.FindGlobal(ctx, options.Identifier, options.Domain)
		if err != nil {
			return nil, err
		}
		if found != nil {
			return &unlockTarget{
				repositoryID: found.RepositoryID, worktreeID: found.WorktreeID, name: found.Slug,
				host: layout.WorktreeHost(found.Slug, found.Domain),
			}, nil
		}
		// A worktree whose first `up` died before it registered, and whose
		// checkout may be gone too, is still named by its UUID in the locks it left.
		repositoryID, orphanFound, err := findOrphanWorktree(cctx.ControlHome, options.Identifier)
		if err != nil {
			return nil, err
		}
		if orphanFound {
			return &unlockTarget{repositoryID: repositoryID, worktreeID: options.Identifier, name: options.Identifier}, nil
		}
		where := ""
		if options.Domain != "" {
			where = " under " + options.Domain
		}
		byUUID := ""
		if uuidPattern.MatchString(options.Identifier) {
			byUUID = ", and no lock is held for a worktree of that UUID"
		}
		return nil, clierr.Newf("No worktree is registered as %s%s%s.", options.Identifier, where, byUUID)
	}

	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, clierr.Newf(
			"This checkout has never been brought up, so it holds no lock. Name another with '%s worktree unlock <slug|uuid>'.", cctx.CommandName)
	}
	record, err := cctx.Registry.Store(identity.RepositoryID).Find(identity.WorktreeID)
	if err != nil {
		return nil, err
	}
	if record != nil {
		return &unlockTarget{
			repositoryID: identity.RepositoryID, worktreeID: identity.WorktreeID, name: record.Slug,
			host: layout.WorktreeHost(record.Slug, record.Domain),
		}, nil
	}
	return &unlockTarget{repositoryID: identity.RepositoryID, worktreeID: identity.WorktreeID, name: identity.Root}, nil
}

// scopeNeedsClearing is a read-only look at whether scope has anything
// lock.Clear would actually clear: an unreleased sidecar record, or a
// TypeScript-era `.lock` leftover — checked only to decide whether the
// "checking the holder" line below is worth printing before the real,
// authoritative check Clear itself makes.
func scopeNeedsClearing(scope lock.Scope) (bool, error) {
	described, err := lock.Describe(scope, nil)
	if err != nil {
		return false, err
	}
	if described.Record != nil && described.Record.ReleasedAt == nil {
		return true, nil
	}
	exists, err := files.PathExists(scope.Target + ".lock")
	if err != nil {
		return false, err
	}
	return exists, nil
}

// findOrphanWorktree scans this control home's worktree locks for one whose
// UUID matches identifier, under any repository: a worktree that never
// registered still leaves its lock file (and, for a TypeScript-era holder,
// its `.lock` directory) named by its own UUID.
func findOrphanWorktree(controlHome, identifier string) (repositoryID string, found bool, err error) {
	worktreesDir := filepath.Join(layout.ControlLayout(controlHome).LocksDir, "worktrees")
	repositories, err := os.ReadDir(worktreesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, repository := range repositories {
		if !repository.IsDir() || !uuidPattern.MatchString(repository.Name()) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(worktreesDir, repository.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, err
		}
		for _, entry := range entries {
			if worktreeIDOf(entry.Name()) == identifier {
				return repository.Name(), true, nil
			}
		}
	}
	return "", false, nil
}

// worktreeIDOf strips every suffix a worktree lock's directory entry can
// carry — the development lock's ".dev", a lock's own sidecar record
// (".holder"), and a TypeScript-era lock directory (".lock") — leaving the
// worktree UUID underneath, whichever of them named is.
func worktreeIDOf(name string) string {
	for {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".holder"), ".lock"), ".dev")
		if trimmed == name {
			return name
		}
		name = trimmed
	}
}
