package registry

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/stateformat"
	"golang.org/x/sys/unix"
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

// maxMarkerBytes bounds what Removing reads: a marker is one short line, so
// anything longer is not one this build wrote.
const maxMarkerBytes = 256

var (
	errMarkerSymlink    = errors.New("a symbolic link")
	errMarkerNotRegular = errors.New("not a regular file")
	errMarkerTooLarge   = errors.New("larger than a marker")
)

// readMarker reads the removal marker at path defensively: opened without
// following a symbolic link and without blocking (a FIFO swapped in after any
// earlier look cannot hang the open), required to be a regular file by the
// descriptor itself, and read no further than maxMarkerBytes (errMarkerTooLarge
// past that), so a planted file cannot hang or exhaust a command that reads
// every worktree's state.
func readMarker(path string) ([]byte, error) {
	return readSmallFile(path, maxMarkerBytes, false)
}

// readSmallFile reads a small file that every worktree's state read may touch,
// without letting a planted one hang or exhaust it: opened without blocking (a
// FIFO cannot hang the open), required to be a regular file by the descriptor
// itself (errMarkerNotRegular), and read no further than limit bytes
// (errMarkerTooLarge past that). With follow false a symbolic link is refused
// (errMarkerSymlink) rather than followed.
func readSmallFile(path string, limit int, follow bool) ([]byte, error) {
	flags := os.O_RDONLY | unix.O_NONBLOCK
	if !follow {
		flags |= unix.O_NOFOLLOW
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		if !follow && errors.Is(err, syscall.ELOOP) {
			return nil, errMarkerSymlink
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errMarkerNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errMarkerTooLarge
	}
	return data, nil
}

// markerKind is what looking at a removal marker found.
type markerKind int

const (
	// markerAbsent is no marker: no removal.
	markerAbsent markerKind = iota
	// markerCurrent is a marker this build wrote.
	markerCurrent
	// markerNewer is one a newer treerig wrote: never touched.
	markerNewer
	// markerUnusable is a file there that is no marker this build wrote
	// (garbage, a foreign format, oversized, a symbolic link or another kind of
	// file): `up` and Removing refuse it, and an explicit removal deletes it.
	markerUnusable
	// markerFailed is a marker that could not be looked at (a permission
	// error, say): nothing can be said about it.
	markerFailed
)

// examineMarker looks at worktreeID's removal marker and says what it is, with
// the error Removing gives for each kind that is not absent or current.
func (s *Store) examineMarker(worktreeID string) (markerKind, error) {
	file := s.removalMarkerPath(worktreeID)
	data, err := readMarker(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return markerAbsent, nil
	case errors.Is(err, errMarkerSymlink):
		return markerUnusable, clierr.Newf("Removal marker must be a regular file, not a symbolic link: %s", file)
	case errors.Is(err, errMarkerNotRegular):
		return markerUnusable, clierr.Newf("Removal marker must be a regular file: %s", file)
	case errors.Is(err, errMarkerTooLarge):
		data = nil
	case err != nil:
		return markerFailed, err
	}
	first, _, _ := strings.Cut(strings.TrimSuffix(string(data), "\n"), "\n")
	switch verdict, found := stateformat.Line(first, stateformat.RemovalMarker); verdict {
	case stateformat.FromNewer:
		return markerNewer, stateformat.Newer("use", "the removal marker "+file, found, stateformat.RemovalMarker)
	case stateformat.Current:
		if string(data) == RemovalMarkerContents() {
			return markerCurrent, nil
		}
	}
	return markerUnusable, clierr.Newf("Removal marker is not in a format %s wrote. It only records that a removal began: delete it, "+
		"and run `worktree remove` again if the worktree is to go: %s", brand.Name, file)
}

// Removing reports whether a removal of worktreeID was started and has not
// finished. A marker of a newer format is refused (stateformat.Newer), and one
// that is not a marker this build wrote is an error naming the file: neither
// is read as "no removal", and neither is touched. (`worktree remove` itself
// clears an unusable one: Store.Remove.)
func (s *Store) Removing(worktreeID string) (bool, error) {
	kind, err := s.examineMarker(worktreeID)
	return kind == markerCurrent, err
}

// CheckRemovable is the early look `worktree remove` takes at worktreeID's
// removal marker, before it plans or asks anything: a marker of a newer format,
// or one that cannot be looked at, is an error; every other state (none, current,
// or one this build cannot use, which the removal itself clears) is fine.
func (s *Store) CheckRemovable(worktreeID string) error {
	switch kind, err := s.examineMarker(worktreeID); kind {
	case markerNewer, markerFailed:
		return err
	}
	return nil
}

// MarkRemoving writes worktreeID's removal marker under the registry lock,
// refusing if the registration is missing or no longer belongs to
// expectedSlug, and refusing a marker that is already there and is not
// current (a newer one, or one this build did not write). Writing a marker
// that is already current changes nothing.
func (s *Store) MarkRemoving(ctx context.Context, worktreeID, expectedSlug string, writer Writer) error {
	_, err := s.markRemoving(ctx, worktreeID, expectedSlug, writer, false)
	return err
}

// BeginRemoval is MarkRemoving for an explicit `worktree remove`, the way out
// of a marker this build cannot use: a file there that is no marker it wrote
// (garbage, a foreign format, oversized, a symbolic link or another kind of
// file) is replaced by a current marker, atomically and without opening it, and
// reported as a *ClearedMarker so the user is told. A marker of a newer format
// is still refused and never touched.
func (s *Store) BeginRemoval(ctx context.Context, worktreeID, expectedSlug string, writer Writer) (*ClearedMarker, error) {
	return s.markRemoving(ctx, worktreeID, expectedSlug, writer, true)
}

// markRemoving is the writer of both; replaceUnusable says whether a marker
// that is no marker this build wrote is replaced or refused.
func (s *Store) markRemoving(ctx context.Context, worktreeID, expectedSlug string, writer Writer, replaceUnusable bool) (*ClearedMarker, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return nil, err
	}
	var cleared *ClearedMarker
	err := s.withRegistryLock(ctx, writer, func(context.Context) error {
		if _, _, err := s.readOwnedRecord(worktreeID, expectedSlug); err != nil {
			return err
		}
		switch kind, err := s.examineMarker(worktreeID); {
		case kind == markerCurrent:
			return nil
		case kind == markerUnusable && replaceUnusable:
			file := s.removalMarkerPath(worktreeID)
			cleared = &ClearedMarker{File: file, Reason: err}
			if err := clearNonRegular(file); err != nil {
				return err
			}
		case kind != markerAbsent:
			return err
		}
		return files.AtomicWrite(s.removalMarkerPath(worktreeID), []byte(RemovalMarkerContents()), 0o600)
	})
	if err != nil {
		return nil, err
	}
	return cleared, nil
}

// clearNonRegular deletes what is at file when it is not a regular file, so
// the atomic write that replaces an unusable marker has somewhere to rename
// to: a file can't be renamed over a directory. os.Remove takes away that one
// entry and never follows it, so a symbolic link goes but not its target, and
// a FIFO, a device or an empty directory goes too. A directory with something
// in it is refused rather than emptied: what is in it is not treerig's to
// delete. A regular file is left for the rename to replace.
func clearNonRegular(file string) error {
	info, err := os.Lstat(file)
	if err != nil || info.Mode().IsRegular() {
		return nil
	}
	if err := os.Remove(file); err != nil {
		if info.IsDir() {
			return clierr.Newf("The removal marker's place is taken by a directory with something in it. Delete it by hand, then run this again: %s", file)
		}
		return err
	}
	return nil
}

// ClearRemoving takes worktreeID's removal marker away under the registry
// lock, for an `up` that brings the worktree back. There being no marker is
// not an error; any marker this build cannot read (newer, foreign, malformed,
// not a regular file) is refused and left alone.
func (s *Store) ClearRemoving(ctx context.Context, worktreeID string, writer Writer) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		// Only a marker that reads as current is deleted: a newer, foreign,
		// oversized or non-regular one is refused and left alone.
		return s.dropRemovalMarker(worktreeID)
	})
}
