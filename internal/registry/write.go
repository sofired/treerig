package registry

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
)

// Writer is who is writing: named to whoever waits on the registry lock.
type Writer struct {
	Logger     logger.Logger
	Invocation string
}

// withRegistryLock runs write under the machine-wide registry lock. It guards
// only milliseconds of JSON, so it polls quickly and waits patiently.
//
// write gets the lock's context, cancelled if the lock is lost; a write that
// finished while the lock was lost fails with why, as the TypeScript
// assertHealthy does. A release that fails as well rides along in Suppressed.
func (s *Store) withRegistryLock(ctx context.Context, writer Writer, write func(context.Context) error) (err error) {
	held := lock.New(lock.RegistryScope(s.ControlHome), lock.Options{
		Timeout: lock.RegistryTimeout,
		Retry:   lock.RegistryRetry,
		Holder:  writer.Invocation,
		Logger:  writer.Logger,
	})
	heldCtx, err := held.Acquire(ctx, writer.Invocation)
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

// withSuppressed keeps primary as the reported error, with also attached.
func withSuppressed(primary, also error) error {
	ce, ok := primary.(*clierr.Error)
	if !ok {
		ce = &clierr.Error{Message: primary.Error(), ExitCode: clierr.Code(primary), Cause: primary}
	}
	ce.Suppressed = append(ce.Suppressed, also)
	return ce
}

// recordJSON is the record as written, in the TypeScript build's key order.
type recordJSON struct {
	Version        int     `json:"version"`
	RepositoryID   string  `json:"repositoryId"`
	WorktreeID     string  `json:"worktreeId"`
	Slug           string  `json:"slug"`
	Domain         string  `json:"domain"`
	LabelNamespace *string `json:"labelNamespace,omitempty"`
	Initialization string  `json:"initialization"`
}

func (s *Store) setInitialization(ctx context.Context, worktreeID, expectedSlug string, writer Writer, state WorktreeInitialization) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID)); err != nil {
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

// Remove deletes a registration, reporting whether there was one to delete.
// A record that now names another slug changed concurrently, and is kept.
func (s *Store) Remove(ctx context.Context, worktreeID, expectedSlug string, writer Writer) (bool, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID)); err != nil {
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
