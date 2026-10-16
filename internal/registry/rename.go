package registry

import (
	"context"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
)

// Migrate moves what is derived from a worktree's slug (route files, the
// index card) from the hostname of previous to that of renamed. Store.Rename
// calls it after the new slug is proven free and before the record changes.
// It must be idempotent, and must derive its result from the record it is
// given and not from a replacement of the old hostname by the new one: a
// rename interrupted after it ran may be retried with a different slug.
type Migrate func(previous, renamed RegisteredWorktree) error

// Describe names a worktree for a refusal that says who holds a slug: its
// last-known path, say. A nil Describe, or an empty answer, names the
// worktree by its ID.
type Describe func(RegisteredWorktree) string

// Rename gives worktreeID's record the slug newSlug, keeping everything else
// it holds: the identity, the domain, the label namespace, the
// initialization state and the pinned ports, which are keyed by the worktree
// and not by its hostname.
//
// The whole decision happens under the registry lock, as Ensure's does: the
// record is read, every registry on the machine is scanned, the slug is
// checked to be free in the record's domain, migrate runs, and the record is
// replaced atomically. The record is replaced last. It is the commit point,
// so a crash before it leaves the old slug registered, with migrate's work
// possibly done, and a retry (with any slug) redoes it from the record; a
// crash after it leaves only the record, which every later `up` re-derives
// the rest from.
//
// expectedSlug guards against a concurrent rename: a record that now names
// another slug is refused, as in MarkPending. Renaming to the slug the
// record already holds changes nothing and does not call migrate. describe
// names the holder of a taken slug in the refusal. Like
// Ensure, a rename refuses while an unreadable record might be occupying the
// hostname.
func (s *Store) Rename(ctx context.Context, worktreeID, expectedSlug, newSlug string, writer Writer, migrate Migrate, describe Describe) (RegisteredWorktree, error) {
	if err := ValidateSlug(newSlug); err != nil {
		return RegisteredWorktree{}, err
	}
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return RegisteredWorktree{}, err
	}
	var renamed RegisteredWorktree
	err := s.withRegistryLock(ctx, writer, func(ctx context.Context) error {
		file, stored, err := s.readOwnedRecord(worktreeID, expectedSlug)
		if err != nil {
			return err
		}
		renamed = stored.Registration
		if stored.Registration.Slug == newSlug {
			return nil
		}
		scan, err := ScanRegistrations(s.ControlHome)
		if err != nil {
			return err
		}
		if len(scan.Problems) > 0 {
			return refuseUnreadable("a hostname",
				"a record that cannot be read may already hold the hostname you asked for.", scan.Problems)
		}
		for _, record := range scan.Records {
			// This worktree's own record is never a rival for the slug it is
			// about to leave; the held-slug case returned above, so it is
			// listed here only under its old slug.
			if record.WorktreeID != worktreeID && record.Domain == stored.Registration.Domain && record.Slug == newSlug {
				holder := ""
				if describe != nil {
					holder = describe(record)
				}
				if holder == "" {
					holder = record.WorktreeID
				}
				return clierr.Newf("The slug %q is held in %s by %s. Choose another slug.", newSlug, stored.Registration.Domain, holder)
			}
		}
		renamed.Slug = newSlug
		if migrate != nil {
			if err := migrate(stored.Registration, renamed); err != nil {
				return err
			}
		}
		return files.AtomicJSON(file, recordFor(renamed, stored.Initialization))
	})
	if err != nil {
		return RegisteredWorktree{}, err
	}
	return renamed, nil
}
