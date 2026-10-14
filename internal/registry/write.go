package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/stateformat"
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
// only short writes, so it polls quickly and waits patiently. Most are
// milliseconds of JSON; a port allocation (SyncPorts) also scans every
// registration and probes candidate ports, which ports.Pick bounds at a
// fixed number of probes per call.
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

// recordJSON is the record as written, in a fixed key order. Nothing but
// recordFor constructs one: every writer starts from a RegisteredWorktree,
// so a field it does not know about is carried, not erased (#119).
type recordJSON struct {
	Version        int             `json:"version"`
	RepositoryID   string          `json:"repositoryId"`
	WorktreeID     string          `json:"worktreeId"`
	Slug           string          `json:"slug"`
	Domain         string          `json:"domain"`
	LabelNamespace *string         `json:"labelNamespace,omitempty"`
	Initialization string          `json:"initialization"`
	Ports          map[string]int  `json:"ports,omitempty"`
	HostRoutes     []hostRouteJSON `json:"hostRoutes,omitempty"`
}

// recordFor is the one record builder: every registry writer goes through
// it. It is total over RegisteredWorktree (TestRecordCarriesEveryField
// fills each field by reflection), so a writer that changes one field of a
// registration and passes the rest through cannot drop the others.
func recordFor(registration RegisteredWorktree, initialization WorktreeInitialization) recordJSON {
	return recordJSON{
		Version:        stateformat.Registration,
		RepositoryID:   registration.RepositoryID,
		WorktreeID:     registration.WorktreeID,
		Slug:           registration.Slug,
		Domain:         registration.Domain,
		LabelNamespace: registration.LabelNamespace,
		Initialization: string(initialization),
		Ports:          nonEmptyPorts(registration.Ports),
		HostRoutes:     hostRoutesJSON(registration.HostRoutes),
	}
}

// readOwnedRecord reads worktreeID's record for a writer that holds the
// registry lock, refusing if it is missing or its slug no longer matches
// expectedSlug — the same reconciliation guard Ensure itself applies.
func (s *Store) readOwnedRecord(worktreeID, expectedSlug string) (string, *storedRecord, error) {
	file := s.RecordPath(worktreeID)
	exists, err := files.PathExists(file)
	if err != nil {
		return "", nil, err
	}
	if !exists {
		return "", nil, clierr.Newf("Worktree registration is missing: %s", file)
	}
	stored, err := s.readStoredRecord(file)
	if err != nil {
		return "", nil, err
	}
	if stored.Registration.Slug != expectedSlug {
		return "", nil, clierr.Newf("Worktree %s no longer owns registration %s", expectedSlug, file)
	}
	return file, stored, nil
}

// setInitialization writes state into worktreeID's record under the registry
// lock, refusing if the record is missing or its slug no longer matches
// expectedSlug, and doing nothing if the record already names state.
func (s *Store) setInitialization(ctx context.Context, worktreeID, expectedSlug string, writer Writer, state WorktreeInitialization) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		file, stored, err := s.readOwnedRecord(worktreeID, expectedSlug)
		if err != nil {
			return err
		}
		if stored.Initialization == state {
			return nil
		}
		return files.AtomicJSON(file, recordFor(stored.Registration, state))
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
			relabelled := stored.Registration
			relabelled.LabelNamespace = labelNamespace
			if err := files.AtomicJSON(file, recordFor(relabelled, stored.Initialization)); err != nil {
				return err
			}
			result := EnsuredWorktree{RegisteredWorktree: relabelled}
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
			return refuseUnreadable("a hostname",
				"a record that cannot be read is one whose Docker resources removal can no longer find.", scan.Problems)
		}
		var others []RegisteredWorktree
		for _, record := range scan.Records {
			if record.WorktreeID != worktreeID {
				others = append(others, record)
			}
		}
		preferred := ""
		if stored != nil {
			// A rename or a re-domain changes the hostname, never the ports
			// or the host routes.
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
		// Starts from the stored record, so a field this rewrite does not
		// mean to change (ports, host routes) is carried, not erased.
		written := RegisteredWorktree{RepositoryID: s.RepositoryID, WorktreeID: worktreeID}
		if stored != nil {
			written = stored.Registration
		}
		written.Slug, written.Domain, written.LabelNamespace = slug, domain, namespace
		if err := files.AtomicJSON(file, recordFor(written, initState)); err != nil {
			return err
		}

		result := EnsuredWorktree{RegisteredWorktree: written}
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

// refuseUnreadable is the refusal to allocate what (a hostname, a port)
// while the registry holds records that could not be read: an unreadable
// record may be occupying it. consequence says why that matters, and each
// problem is listed with its reason beside its file.
func refuseUnreadable(what, consequence string, problems []RegistryProblem) error {
	var listing strings.Builder
	allNewer, anyNewer := len(problems) > 0, false
	for _, problem := range problems {
		fmt.Fprintf(&listing, "  %s: %s\n", problem.File, problem.Reason)
		allNewer = allNewer && problem.Newer
		anyNewer = anyNewer || problem.Newer
	}
	// A record a newer berth wrote is intact, only unreadable here, and may
	// hold what is being allocated: moving it aside would be the wrong advice.
	// Only files that are damaged or foreign are told to be moved.
	advice := "Move a file aside once you know what it is: " + consequence
	switch {
	case allNewer:
		advice = "Upgrade " + brand.Name + " to read them: " + consequence
	case anyNewer:
		advice = "Upgrade " + brand.Name + " to read the files that say so, and move any other file aside once you know what it is: " + consequence
	}
	return clierr.Newf(
		"Refusing to allocate %s while the registry has unreadable records:\n%s"+
			"Each reason is beside its file above. %s",
		what, listing.String(), advice,
	)
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
