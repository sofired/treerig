package registry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/stateformat"
)

// The removal marker (docs/design/state.md). `worktree remove` writes it
// beside the registration when it starts tearing the worktree down, before it
// deletes anything, and Store.Remove deletes it with the registration, so a
// removal that was interrupted leaves it behind and the worktree reads as
// `removing` until the removal is run again.
//
// It is a store of its own, not a field of the registration: a registration
// has a closed shape, so a build that does not know a new key would read every
// registration as malformed. A build that does not know the marker never opens
// a file with this name, and a removal by such a build (a 0.1.x pin) writes
// none.

// RemovalMarkerContents is what a removal marker holds: its format line.
func RemovalMarkerContents() string {
	return stateformat.FormatLine(stateformat.RemovalMarker) + "\n"
}

// removalMarkerPath is where worktreeID's removal marker lives.
func (s *Store) removalMarkerPath(worktreeID string) string {
	return layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID).RemovalPath
}

// Removing reports whether a removal of worktreeID was started and has not
// finished. A marker of a newer format is refused (stateformat.Newer), and one
// that is not a marker this build wrote is an error naming the file: neither
// is read as "no removal", and neither is touched.
func (s *Store) Removing(worktreeID string) (bool, error) {
	file := s.removalMarkerPath(worktreeID)
	info, err := os.Lstat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, clierr.Newf("Removal marker must be a regular file, not a symbolic link: %s", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	first, _, _ := strings.Cut(strings.TrimSuffix(string(data), "\n"), "\n")
	switch verdict, found := stateformat.Line(first, stateformat.RemovalMarker); verdict {
	case stateformat.FromNewer:
		return false, stateformat.Newer("use", "the removal marker "+file, found, stateformat.RemovalMarker)
	case stateformat.Current:
		if string(data) == RemovalMarkerContents() {
			return true, nil
		}
	}
	return false, clierr.Newf("Removal marker is not in a format %s wrote: %s. It only records that a removal began: delete it, "+
		"and run `worktree remove` again if the worktree is to go.", brand.Name, file)
}

// MarkRemoving writes worktreeID's removal marker under the registry lock,
// refusing if the registration is missing or no longer belongs to
// expectedSlug, and refusing a marker of a newer format that is already
// there. Writing a marker that is already current changes nothing.
func (s *Store) MarkRemoving(ctx context.Context, worktreeID, expectedSlug string, writer Writer) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		if _, _, err := s.readOwnedRecord(worktreeID, expectedSlug); err != nil {
			return err
		}
		marked, err := s.Removing(worktreeID)
		if err != nil {
			return err
		}
		if marked {
			return nil
		}
		return files.AtomicWrite(s.removalMarkerPath(worktreeID), []byte(RemovalMarkerContents()), 0o600)
	})
}

// ClearRemoving takes worktreeID's removal marker away under the registry
// lock, for an `up` that brings the worktree back. There being no marker is
// not an error; a marker of a newer format is refused and left alone.
func (s *Store) ClearRemoving(ctx context.Context, worktreeID string, writer Writer) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		if _, err := s.Removing(worktreeID); err != nil && errors.Is(err, stateformat.ErrNewer) {
			return err
		}
		if err := os.Remove(s.removalMarkerPath(worktreeID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}
