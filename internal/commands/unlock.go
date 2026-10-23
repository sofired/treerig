package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
)

// The one way a dead holder's worktree lock is cleared, built on the flock
// lock layer (see internal/lock's package comment). It never guesses:
// lock.Clear only ever marks a holder's record released once flock(2)
// itself proves nobody holds the lock and its record's identity checks
// prove that holder gone — or, with --holder-unverifiable, once the person
// running the command has said so. It takes no lock of its own: both of a
// worktree's locks are probed read-only first (lock.ProbeHeld), so an
// active one is always refused before anything is cleared, never partway
// through.

// UnlockOptions are `worktree unlock`'s argument and flags.
type UnlockOptions struct {
	// Identifier names the worktree (registry.Registry.Resolve); the worktree
	// the command runs in when empty. Must
	// be empty when Registry or Router is set (#21): those name a single
	// machine-wide lock, not a worktree, and UnlockGlobal refuses a
	// non-empty Identifier or Domain rather than silently ignoring it.
	Identifier string
	Domain     string
	// Registry clears the machine-wide worktree registry lock instead of a
	// worktree's — --registry (#21). Mutually exclusive with Router and
	// with a non-empty Identifier or Domain.
	Registry bool
	// Router clears the shared router's lock instead of a worktree's —
	// --router (#21). Mutually exclusive with Registry and with a
	// non-empty Identifier or Domain.
	Router bool
	// Shared clears the shared-services lock of the repository the worktree
	// (this checkout, or Identifier) belongs to instead of the worktree's own
	// locks — --shared. Mutually exclusive with Registry and Router.
	Shared bool
	// AllowUnverifiable accepts a holder this machine cannot check (another
	// machine or PID namespace, or no record) — --holder-unverifiable.
	AllowUnverifiable bool
}

// ClearedLock is one lock UnlockWithContext or UnlockGlobal cleared.
type ClearedLock struct {
	// Kind is "operation" or "development" (lock.WorktreeScopes), or
	// "registry" or "router" (#21).
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

// unlockTarget is the worktree to unlock, named the way its locks are.
type unlockTarget struct {
	repositoryID string
	worktreeID   string
	name         string
	// host is "" for a worktree not (or no longer) registered.
	host string
}

// Unlock is `worktree unlock`. --registry and --router (#21) name a
// machine-wide lock rather than a worktree, so they run before any context is
// built — UnlockGlobal needs none, the same way `doctor` and the router verbs
// read Env directly. Every other form uses ContextWithoutConfig, so none needs
// a treerig.toml: a worktree named by UUID, host, path or slug is unlocked from anywhere,
// and a treerig.toml that does not parse is warned about and set aside.
func Unlock(ctx context.Context, env *Env, options UnlockOptions) error {
	if options.Shared && (options.Registry || options.Router) {
		return clierr.New("Not clearing any lock: --shared cannot be combined with --registry or --router.")
	}
	if options.Registry || options.Router {
		_, err := UnlockGlobal(ctx, env, options)
		return err
	}
	// Recovery works from labels and records, so a missing or broken
	// treerig.toml must not stand in its way, and a worktree named by slug or
	// UUID can be unlocked from anywhere.
	cctx, err := ContextWithoutConfig(env)
	if err != nil {
		return err
	}
	if options.Shared {
		_, err = UnlockSharedWithContext(ctx, cctx, options)
		return err
	}
	_, err = UnlockWithContext(ctx, cctx, options)
	return err
}

// UnlockGlobal is `worktree unlock --registry` or `--router` (#21): the
// same guarded recovery UnlockWithContext gives a worktree's own locks
// (lock.ProbeHeld, then lock.Clear — never cleared on age alone, never
// assumed gone), pointed instead at the one machine-wide lock the flags
// name. It needs no project config and no CommandContext: the registry and
// router locks exist, and can go stale, on a machine with no checkout
// registered at all.
func UnlockGlobal(ctx context.Context, env *Env, options UnlockOptions) (*UnlockResult, error) {
	if options.Registry == options.Router {
		// UnlockOptions's zero value has both false; Unlock never calls
		// this function with both false, so reaching this branch anyway
		// means both were set — this validates it explicitly rather than
		// trust that invariant silently.
		return nil, clierr.New("Not clearing any lock: pass exactly one of --registry or --router.")
	}
	which := "--registry"
	kind := "registry"
	if options.Router {
		which, kind = "--router", "router"
	}
	if options.Identifier != "" || options.Domain != "" {
		return nil, clierr.Newf(
			"Not clearing any lock: %s clears a single machine-wide lock, and takes no worktree identifier or --domain.", which)
	}

	var scope lock.Scope
	if options.Registry {
		home, userHome, err := checkedHome(env.Getenv)
		if err != nil {
			return nil, err
		}
		controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
		if err != nil {
			return nil, err
		}
		// Before the registry lock file is ever opened: securing a
		// control home that is not there yet would create it — an unlock
		// command has no business doing that (below finds nothing to
		// unlock instead, exactly as if the lock itself were simply
		// absent). Only one that already exists gets the same guard
		// router.SecureState gives a router lock: files.
		// SecureControlDirectories refuses a symlinked or foreign one, the
		// same way every mutating verb secures it before touching
		// anything inside (ProbeHeld and lock.Clear open scope.Target
		// directly, with nothing of their own to catch a symlink there
		// first). lstatExists never follows a symlink at controlHome's own
		// final component, so one counts as existing, and then fails the
		// check that follows.
		exists, err := lstatExists(controlHome)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := files.SecureControlDirectories(layout.ControlLayout(controlHome), userHome); err != nil {
				return nil, err
			}
		}
		scope = lock.RegistryScope(controlHome)
	} else {
		state, err := resolveRouterState(env.Getenv)
		if err != nil {
			return nil, err
		}
		// Same reasoning as the registry branch above: router.SecureState
		// creates the router's state tree (secureDirectories' own
		// os.MkdirAll) as readily as it secures one already there, so it
		// is only called once lstatExists confirms the state directory
		// itself is real — otherwise there is nothing for it to secure,
		// and below finds nothing to unlock without ever having touched
		// the filesystem. Once it exists, this is the same guard RouterUp
		// (router.go:79) and newOverview (overview.go) both take before
		// ever touching theirs.
		exists, err := lstatExists(state.StateDir)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := router.SecureState(state); err != nil {
				return nil, err
			}
		}
		// No router.Router to ask for its own ProjectName, the same
		// position newOverview is in (overview.go): routerLockScope
		// resolves router.ResolveProjectName()'s default.
		scope = routerLockScope(state, "")
	}
	return clearOneScope(ctx, env.Logger, env.Table, scope, kind, "the "+scope.Description, clearWording, options)
}

// clearWords is how clearOneScope speaks of the lock it is clearing.
type clearWords struct {
	// nothing, checkingWithCommands, checking and releasedMeanwhile take the
	// lock's label; unlocked takes the label and the scope's description.
	nothing, checkingWithCommands, checking, releasedMeanwhile string
	unlocked                                                   func(label, description string) string
}

// clearWording is the wording for the registry and router locks, whose label
// is the lock's own name.
var clearWording = clearWords{
	nothing:              "Nothing to unlock: no lock is held for %s.",
	checkingWithCommands: "Checking that the holder of %s's lock, and every command it started on this machine, is gone.",
	checking:             "Checking that the holder of %s's lock is gone.",
	releasedMeanwhile:    "Nothing to unlock: the lock on %s was released while it was being checked.",
	unlocked: func(label, description string) string {
		return fmt.Sprintf("Unlocked %s: cleared the %s lock.", label, description)
	},
}

// sharedClearWording is the wording for a repository's shared-services lock,
// whose label already says it is a lock ("the shared-services lock of <host>").
var sharedClearWording = clearWords{
	nothing:              "Nothing to unlock: no one holds %s.",
	checkingWithCommands: "Checking that the holder of %s, and every command it started on this machine, is gone.",
	checking:             "Checking that the holder of %s is gone.",
	releasedMeanwhile:    "Nothing to unlock: %s was released while it was being checked.",
	unlocked:             func(label, _ string) string { return fmt.Sprintf("Unlocked %s.", label) },
}

// clearOneScope is the guarded recovery of a single lock — the registry's,
// the router's or a repository's shared lock — which has no worktree to name:
// refuse while it is actively held, say so when there is nothing to clear,
// and otherwise hand it to lock.Clear, judging the holder gone (or, with
// options.AllowUnverifiable, accepting it on the person's word) before
// marking it released. label names the lock in the messages.
func clearOneScope(ctx context.Context, log logger.Logger, table procid.Table, scope lock.Scope, kind, label string, words clearWords, options UnlockOptions) (*UnlockResult, error) {
	if table == nil {
		table = procid.Platform()
	}
	held, refusal, err := lock.ProbeHeld(ctx, scope, table)
	if err != nil {
		return nil, err
	}
	if held {
		return nil, refusal
	}

	stale, err := scopeNeedsClearing(scope)
	if err != nil {
		return nil, err
	}
	if !stale {
		log.Info(fmt.Sprintf(words.nothing, label))
		return &UnlockResult{Name: label}, nil
	}
	// The registry lock guards only brief writes and carries no process
	// markers of its own (#21), so — unlike the router's own Compose
	// project — it is never scanned for anything it started.
	if scope.Markers.Project != "" || scope.Markers.WorktreeID != "" {
		log.Info(fmt.Sprintf(words.checkingWithCommands, label))
	} else {
		log.Info(fmt.Sprintf(words.checking, label))
	}

	cleared, clearErr := lock.Clear(ctx, []lock.Scope{scope}, lock.ClearOptions{
		Table: table, AllowUnverifiable: options.AllowUnverifiable, SuggestUnverifiableFlag: true,
	})

	result := &UnlockResult{Name: label}
	var verified, unverified []string
	for _, sr := range cleared.Scopes {
		if !sr.Cleared {
			continue
		}
		result.Cleared = append(result.Cleared, ClearedLock{Kind: kind, Description: sr.Scope.Description, Target: sr.Scope.Target})
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
		log.Info(fmt.Sprintf(words.releasedMeanwhile, label))
		return result, nil
	}
	if cleared.Proof != "" {
		verified = append(verified, cleared.Proof)
	}
	for _, line := range verified {
		log.Info(fmt.Sprintf("Verified: %s.", line))
	}
	for _, line := range unverified {
		log.Warn(fmt.Sprintf("Not verified, on your word (--holder-unverifiable): %s.", line))
	}
	log.Info(words.unlocked(label, scope.Description))
	return result, nil
}

// UnlockSharedWithContext is `worktree unlock --shared`: the same guarded
// recovery as a worktree's own locks (lock.ProbeHeld, then lock.Clear, never
// cleared on age alone), pointed at the shared lock of the repository the
// target worktree belongs to (docs/design/routing.md, decision 18). The
// shared project's name is its survivor-scan marker, so a `compose up` the
// dead holder started is looked for before the lock is declared free.
func UnlockSharedWithContext(ctx context.Context, cctx *CommandContext, options UnlockOptions) (*UnlockResult, error) {
	target, err := resolveUnlockTarget(ctx, cctx, options)
	if err != nil {
		return nil, err
	}
	scope := cctx.sharedScope(target.repositoryID)
	label := "the " + scope.Description + " lock of " + target.name
	if target.host != "" {
		label = "the " + scope.Description + " lock of " + target.host
	}
	exists, err := lstatExists(scope.Target)
	if err != nil {
		return nil, err
	}
	if !exists {
		cctx.Logger.Info(fmt.Sprintf(sharedClearWording.nothing, label))
		return &UnlockResult{Name: label}, nil
	}
	table := cctx.Table
	if table == nil {
		table = procid.Platform()
	}
	return clearOneScope(ctx, cctx.Logger, table, scope, "shared", label, sharedClearWording, options)
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

	table := cctx.Table
	if table == nil {
		table = procid.Platform()
	}
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
	// flock(2) proves a lock free the instant it is probed, so this notice
	// promises no further wait.
	cctx.Logger.Info(fmt.Sprintf("Checking that the holder of %s's lock, and every command it started on this machine, is gone.", label))

	if env, ok := cctx.environWorktreeID(); ok && env == target.worktreeID {
		return nil, clierr.New(
			"This command is running inside the worktree's own environment (" + brand.EnvName("WORKTREE_ID") + " is set, as in its hooks), " +
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
	// The one line always added last, once it established no command this
	// worktree started is still running.
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
// here; naming every lock already cleared before the failure is this note's
// own addition. err is returned unchanged when cleared is empty: lock.Clear
// never got as far as committing anything, so there is nothing to preface.
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

// environWorktreeID is TREERIG_WORKTREE_ID from cctx's own process environment.
func (c *CommandContext) environWorktreeID() (string, bool) {
	for _, entry := range c.Environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == brand.EnvName("WORKTREE_ID") {
			return value, true
		}
	}
	return "", false
}

// resolveUnlockTarget is the worktree to unlock: one named anywhere on this
// machine (registry.Registry.Resolve: a UUID, a host, the path to its
// directory, or a slug) — by UUID even if it never registered — or else the
// worktree this command runs in, by its identity markers. Unlocking clears a
// lock, so a path must name the directory itself, not something inside it.
func resolveUnlockTarget(ctx context.Context, cctx *CommandContext, options UnlockOptions) (*unlockTarget, error) {
	if options.Identifier != "" {
		resolved, err := cctx.Registry.Resolve(ctx, options.Identifier, cctx.nameOptions(options.Domain, nil, false))
		if err == nil {
			found := resolved.Record
			return &unlockTarget{
				repositoryID: found.RepositoryID, worktreeID: found.WorktreeID, name: found.Slug,
				host: layout.WorktreeHost(found.Slug, found.Domain),
			}, nil
		}
		if !errors.Is(err, registry.ErrNoWorktree) || !registry.IsUUID(options.Identifier) {
			return nil, err
		}
		// A worktree whose first `up` died before it registered, and whose
		// directory may be gone too, is still named by its UUID in the locks it
		// left. Lock files are named by the lowercase UUID, so the spelling is
		// folded once, for the match and for the lock that is then cleared.
		orphanID := strings.ToLower(options.Identifier)
		repositoryID, orphanFound, orphanErr := findOrphanWorktree(cctx.ControlHome, orphanID)
		if orphanErr != nil {
			return nil, orphanErr
		}
		if orphanFound {
			return &unlockTarget{repositoryID: repositoryID, worktreeID: orphanID, name: orphanID}, nil
		}
		return nil, clierr.Newf("No worktree is registered as %s, and no lock is held for a worktree of that UUID.", options.Identifier)
	}

	flag := ""
	if options.Shared {
		flag = "--shared "
	}
	_, _, inside, err := cctx.Registry.Identities.Repository(ctx, cctx.Registry.Identities.Root)
	if err != nil {
		return nil, err
	}
	if !inside {
		return nil, clierr.Newf(
			"There is no worktree to unlock here: %s is not inside a Git worktree. Name one with '%s worktree unlock %s<worktree>'.", cctx.Cwd, cctx.CommandName, flag)
	}
	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, clierr.Newf(
			"This worktree has never been brought up, so it holds no lock. Name another with '%s worktree unlock %s<worktree>'.", cctx.CommandName, flag)
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

// lstatExists reports whether path is there at all, without following a
// symlink at its own final path component: unlike files.PathExists (which
// os.Stats, and so would call a dangling symlink "not there" and let
// UnlockGlobal skip straight past the secure check that exists to refuse
// exactly that), a symlink here counts as existing — os.Lstat sees it
// without ever resolving it — so the caller always runs that check against
// one, the same way AssertSafeDirectoryPathNamed's own os.Lstat loop
// (internal/files) never follows one either.
func lstatExists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// scopeNeedsClearing is a read-only look at whether scope has anything
// lock.Clear would actually clear: an unreleased sidecar record — checked
// only to decide whether the "checking the holder" line below is worth
// printing before the real, authoritative check Clear itself makes.
func scopeNeedsClearing(scope lock.Scope) (bool, error) {
	described, err := lock.Describe(scope, nil)
	if err != nil {
		return false, err
	}
	return described.Record != nil && described.Record.ReleasedAt == nil, nil
}

// findOrphanWorktree looks up one worktree among OrphanWorktrees by UUID: a
// worktree that never registered still leaves its lock file named by its
// own UUID.
func findOrphanWorktree(controlHome, identifier string) (repositoryID string, found bool, err error) {
	orphans, err := OrphanWorktrees(controlHome)
	if err != nil {
		return "", false, err
	}
	for _, orphan := range orphans {
		if orphan.WorktreeID == identifier {
			return orphan.RepositoryID, true, nil
		}
	}
	return "", false, nil
}

// OrphanWorktree is a worktree whose operation or development lock this
// control home still has a file for, under some repository, but whose
// registration is gone or never existed — nameable at `worktree unlock`
// only by its own UUID (resolveUnlockTarget's own fallback below, and what
// shell completion offers beside every registered slug).
type OrphanWorktree struct {
	RepositoryID string
	WorktreeID   string
}

// OrphanWorktrees scans this control home's worktree locks for every
// worktree UUID one exists under, across every repository: the read-only
// scan (os.ReadDir, no flock, no write) findOrphanWorktree above narrows
// to a single identifier — this generalises it to list every one it would
// find, a deliberate trade of findOrphanWorktree's own single-entry lookup
// for a full scan, so both share one implementation — for `worktree
// unlock`'s own shell completion, which offers a worktree it can still
// recover even though nothing is registered for it. A repository directory
// that vanishes mid-scan (another process finishing its own unlock) is
// skipped, not failed on, the same way findOrphanWorktree always tolerated
// it. An entry that is not itself a worktree UUID once its own suffixes are
// stripped — a stray file like .DS_Store, never one this tool created — is
// skipped too, the same way the repository directory above it already is.
func OrphanWorktrees(controlHome string) ([]OrphanWorktree, error) {
	worktreesDir := filepath.Join(layout.ControlLayout(controlHome).LocksDir, "worktrees")
	repositories, err := os.ReadDir(worktreesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var orphans []OrphanWorktree
	seen := map[OrphanWorktree]bool{}
	for _, repository := range repositories {
		if !repository.IsDir() || !registry.IsUUID(repository.Name()) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(worktreesDir, repository.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			worktreeID := worktreeIDOf(entry.Name())
			if !registry.IsUUID(worktreeID) {
				continue
			}
			candidate := OrphanWorktree{RepositoryID: repository.Name(), WorktreeID: worktreeID}
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			orphans = append(orphans, candidate)
		}
	}
	return orphans, nil
}

// worktreeIDOf strips every suffix a worktree lock's directory entry can
// carry — the development lock's ".dev" and a lock's own sidecar record
// (".holder") — leaving the worktree UUID underneath, whichever of them
// named is.
func worktreeIDOf(name string) string {
	for {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(name, ".holder"), ".dev")
		if trimmed == name {
			return name
		}
		name = trimmed
	}
}
