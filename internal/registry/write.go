package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
)

// Writer is who is writing: named to whoever waits on a lock.
type Writer struct {
	Logger     logger.Logger
	Invocation string
	// UserHome is the caller's own resolved home directory (in production,
	// layout.HomeDir(getenv) from internal/commands' checkedHome, #80),
	// handed to files.SecureRegistrationDirectories's own
	// AssertAdoptableControlHome check ahead of every write here — this
	// package does not read the environment itself for it, only the entry
	// point does.
	UserHome string
}

// withLock runs write with scope held, releasing it whether write succeeds
// or not. write gets the lock's context, cancelled if the lock is lost, so
// a write that finished while the lock was lost fails with why. A release
// that fails as well rides along in Suppressed.
func withLock(ctx context.Context, scope lock.Scope, opts lock.Options, invocation string, write func(context.Context) error) (err error) {
	held := lock.New(scope, opts)
	heldCtx, err := held.Acquire(ctx, invocation)
	if err != nil {
		return err
	}
	defer func() {
		releaseErr := held.Release()
		switch {
		case releaseErr == nil:
		case err == nil:
			err = releaseErr
		default:
			err = withSuppressed(err, releaseErr)
		}
	}()
	if err := write(heldCtx); err != nil {
		return err
	}
	if held.Lost() {
		return held.Err()
	}
	return nil
}

// withRegistryLock runs write under the machine-wide registry lock. It guards
// only milliseconds of JSON, so it polls quickly and waits patiently.
func (s *Store) withRegistryLock(ctx context.Context, writer Writer, write func(context.Context) error) error {
	return withLock(ctx, lock.RegistryScope(s.ControlHome), lock.Options{
		Timeout: lock.RegistryTimeout,
		Retry:   lock.RegistryRetry,
		Holder:  writer.Invocation,
		Logger:  writer.Logger,
	}, writer.Invocation, write)
}

// withWorktreeLock runs write under one worktree's own operation lock (never
// its development-session lock), for WorktreeRegistry.Ensure's outer hold —
// the registry lock write.Ensure takes internally nests inside it, since the
// two are different flock targets.
func withWorktreeLock(ctx context.Context, scope lock.Scope, writer Writer, write func(context.Context) error) error {
	return withLock(ctx, scope, lock.Options{
		Timeout: lock.OperationTimeout,
		Holder:  writer.Invocation,
		Logger:  writer.Logger,
	}, writer.Invocation, write)
}

// withSuppressed keeps primary as the reported error, with also attached.
func withSuppressed(primary, also error) error {
	ce, ok := primary.(*clierr.Error)
	if !ok {
		ce = &clierr.Error{Message: primary.Error(), ExitCode: clierr.Code(primary), Cause: primary}
	}
	ce.Suppressed = append(ce.Suppressed, also)
	return ce
}

// recordJSON is the record as written, in a fixed key order.
type recordJSON struct {
	Version        int     `json:"version"`
	RepositoryID   string  `json:"repositoryId"`
	WorktreeID     string  `json:"worktreeId"`
	Slug           string  `json:"slug"`
	Domain         string  `json:"domain"`
	LabelNamespace *string `json:"labelNamespace,omitempty"`
	Initialization string  `json:"initialization"`
}

// setInitialization writes state into worktreeID's record under the registry
// lock, refusing if the record is missing or its slug no longer matches
// expectedSlug — the same reconciliation guard Ensure itself applies —
// and doing nothing if the record already names state.
func (s *Store) setInitialization(ctx context.Context, worktreeID, expectedSlug string, writer Writer, state WorktreeInitialization) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		file := s.RecordPath(worktreeID)
		exists, err := files.PathExists(file)
		if err != nil {
			return err
		}
		if !exists {
			return clierr.Newf("Worktree registration is missing: %s", file)
		}
		stored, err := s.readStoredRecord(file)
		if err != nil {
			return err
		}
		if stored.Registration.Slug != expectedSlug {
			return clierr.Newf("Worktree %s no longer owns registration %s", expectedSlug, file)
		}
		if stored.Initialization == state {
			return nil
		}
		return files.AtomicJSON(file, recordJSON{
			Version:        1,
			RepositoryID:   stored.Registration.RepositoryID,
			WorktreeID:     stored.Registration.WorktreeID,
			Slug:           stored.Registration.Slug,
			Domain:         stored.Registration.Domain,
			LabelNamespace: stored.Registration.LabelNamespace,
			Initialization: string(state),
		})
	})
}

// MarkPending is set before a removal destroys data, so if it is interrupted
// the next `up` seeds again rather than starting on empty volumes as if
// nothing had happened.
func (s *Store) MarkPending(ctx context.Context, worktreeID, expectedSlug string, writer Writer) error {
	return s.setInitialization(ctx, worktreeID, expectedSlug, writer, Pending)
}

// CompleteInitialization marks a worktree's first successful `up`.
func (s *Store) CompleteInitialization(ctx context.Context, worktreeID, expectedSlug string, writer Writer) error {
	return s.setInitialization(ctx, worktreeID, expectedSlug, writer, Complete)
}

// Allocator picks a slug for a worktree given every other registered
// worktree in the same domain and the slug it already holds, if any
// (""` means none). AllocateSlug is the production one.
type Allocator func(others []RegisteredWorktree, preferred string) (string, error)

// Initializer decides a brand new record's initialization state. Called only
// when a record is actually about to be written — the fast path where a
// record already matches never asks.
type Initializer func() (WorktreeInitialization, error)

// Ensure registers the worktree, or reconciles its record with the domain it
// was called with. The whole decision — read the record, scan every other
// registry, allocate, write — happens under the registry lock, so a rename
// and a concurrent first registration cannot both mint the same hostname.
//
// A registered worktree whose domain already matches never scans; that is
// the path every `dev` takes, and it stays cheap and immune to problems
// elsewhere in the registry. Allocation refuses only when a record it cannot
// read might be occupying a hostname it cannot see.
func (s *Store) Ensure(
	ctx context.Context,
	worktreeID, domain string,
	allocate Allocator,
	writer Writer,
	initialization Initializer,
	labelNamespace *string,
) (*EnsuredWorktree, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return nil, err
	}
	var ensured *EnsuredWorktree
	err := s.withRegistryLock(ctx, writer, func(context.Context) error {
		file := s.RecordPath(worktreeID)
		stored, err := s.readStoredRecord(file)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			stored = nil
		}

		if stored != nil && stored.Registration.Domain == domain {
			if labelNamespace == nil || (stored.Registration.LabelNamespace != nil && *stored.Registration.LabelNamespace == *labelNamespace) {
				ensured = &EnsuredWorktree{RegisteredWorktree: stored.Registration}
				return nil
			}
			// Same hostname, different labels: rewrite in place; nothing to allocate.
			if err := files.AtomicJSON(file, recordJSON{
				Version: 1, RepositoryID: s.RepositoryID, WorktreeID: worktreeID,
				Slug: stored.Registration.Slug, Domain: domain,
				LabelNamespace: labelNamespace, Initialization: string(stored.Initialization),
			}); err != nil {
				return err
			}
			result := EnsuredWorktree{RegisteredWorktree: RegisteredWorktree{
				RepositoryID: s.RepositoryID, WorktreeID: worktreeID,
				Slug: stored.Registration.Slug, Domain: domain, LabelNamespace: labelNamespace,
			}}
			if previous := stored.Registration.LabelNamespace; previous != nil {
				result.PreviousLabelNamespace = previous
			}
			ensured = &result
			return nil
		}

		scan, err := ScanRegistrations(s.ControlHome)
		if err != nil {
			return err
		}
		if len(scan.Problems) > 0 {
			var listing strings.Builder
			for _, problem := range scan.Problems {
				fmt.Fprintf(&listing, "  %s: %s\n", problem.File, problem.Reason)
			}
			return clierr.Newf(
				"Refusing to allocate a hostname while the registry has unreadable records:\n%s"+
					"Each reason is beside its file above. Move a file aside once you know what it is: "+
					"a record that cannot be read is one whose Docker resources removal can no longer find.",
				listing.String(),
			)
		}
		var others []RegisteredWorktree
		for _, record := range scan.Records {
			if record.WorktreeID != worktreeID {
				others = append(others, record)
			}
		}
		preferred := ""
		if stored != nil {
			preferred = stored.Registration.Slug
		}
		slug, err := allocate(others, preferred)
		if err != nil {
			return err
		}
		// A caller that does not know the namespace must not erase one that is recorded.
		namespace := labelNamespace
		if namespace == nil && stored != nil {
			namespace = stored.Registration.LabelNamespace
		}
		var initState WorktreeInitialization
		if stored != nil {
			initState = stored.Initialization
		} else {
			// Only computed when a record is actually written; the fast path never asks.
			initState, err = initialization()
			if err != nil {
				return err
			}
		}
		if err := files.AtomicJSON(file, recordJSON{
			Version: 1, RepositoryID: s.RepositoryID, WorktreeID: worktreeID,
			Slug: slug, Domain: domain, LabelNamespace: namespace, Initialization: string(initState),
		}); err != nil {
			return err
		}

		result := EnsuredWorktree{RegisteredWorktree: RegisteredWorktree{
			RepositoryID: s.RepositoryID, WorktreeID: worktreeID, Slug: slug, Domain: domain, LabelNamespace: namespace,
		}}
		if stored != nil {
			host := layout.WorktreeHost(stored.Registration.Slug, stored.Registration.Domain)
			result.PreviousHost = &host
			if previous := stored.Registration.LabelNamespace; previous != nil && (namespace == nil || *previous != *namespace) {
				result.PreviousLabelNamespace = previous
			}
		}
		ensured = &result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ensured, nil
}

// Remove deletes a registration, reporting whether there was one to delete.
// A record that now names another slug changed concurrently, and is kept.
func (s *Store) Remove(ctx context.Context, worktreeID, expectedSlug string, writer Writer) (bool, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return false, err
	}
	removed := false
	err := s.withRegistryLock(ctx, writer, func(context.Context) error {
		current, err := s.Find(worktreeID)
		if err != nil {
			return err
		}
		if current == nil {
			return nil
		}
		if current.Slug != expectedSlug {
			return clierr.Newf("Worktree %s changed concurrently; its registration was retained.", expectedSlug)
		}
		if err := os.Remove(s.RecordPath(worktreeID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}
