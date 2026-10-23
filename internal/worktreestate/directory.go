package worktreestate

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// Hints for a directory that may or may not be gone: what an unknown worktree
// says is the cause (docs/design/cli-1.0.md, "State words").
const (
	hintParent        = "Its parent directory is missing or empty: is the volume mounted?"
	hintDamaged       = "Git's records for it are damaged: 'git worktree repair' may fix them."
	hintLocked        = "Git has it locked, so its directory may be on a drive that is not connected: connect it, or run 'git worktree unlock'."
	hintNotPrunable   = "Git does not call it prunable (Git 2.31 or newer says so)."
	hintUnidentified  = "Git lists a worktree treerig has no identity for, which might be this one moved."
	hintCouldNotCheck = "treerig couldn't check it: "
)

// evidence is what is known of one worktree's directory: Classify's input and
// the cause when it is not known.
type evidence struct {
	directory worktree.Directory
	hint      string
}

// undetermined is evidence that says only why nothing can be said.
func undetermined(hint string) evidence {
	return evidence{directory: worktree.DirectoryUndetermined, hint: hint}
}

// couldNotCheck is the hint for a check that failed with err.
func couldNotCheck(err error) string {
	return hintCouldNotCheck + text.OneLine(err.Error(), 200) + "."
}

// mountTable reads the mount table, once per Gatherer.
func (g *Gatherer) mountTable() (Mounts, error) {
	g.mountsOnce.Do(func() { g.mounts, g.mountsErr = g.readMounts() })
	return g.mounts, g.mountsErr
}

// directoryOf decides whether a worktree's directory is gone, on positive
// evidence only. entry is what Git lists for the worktree (nil when it lists
// nothing for it), recordedRoot the directory the last `up` saw it in, and
// gitErr why Git could not be asked, if it could not, and slug the
// worktree's, for the remedy a hint names.
//
// The directory is gone when Git answered, lists the entry as prunable and
// not locked, the directory itself is absent, and its parent exists, is not
// empty and is not below an automount placeholder. Anything short of that is
// undetermined, with a hint that names the cause: an unmounted volume's mount
// point is an ordinary empty directory, so an empty or absent parent is the
// signal that the volume may simply not be mounted, and Git also reports an
// existing directory as prunable when its administrative files are gone,
// which is a repair, not a deletion.
func (g *Gatherer) directoryOf(entry *registry.WorktreeEntry, listed []registry.WorktreeEntry, recordedRoot, slug string, gitErr error) evidence {
	switch {
	case gitErr != nil:
		return undetermined(couldNotCheck(gitErr))
	case entry == nil && recordedRoot == "":
		return undetermined("Git does not list it and no directory was recorded for it: run 'treerig worktree remove " + slug + "' if it is gone.")
	case entry == nil:
		// Git lists nothing with this worktree's identity (pruned, or never in
		// this repository): the same checks, on the directory the last `up`
		// saw it in. But an entry whose identity could not be read, or that
		// could not be examined, or that has none and lives elsewhere without
		// being a live, healthy directory, might be this worktree, moved: then
		// nothing can be said. A live unmanaged checkout (a plain `git
		// worktree add`, the main worktree before treerig) was looked at by
		// Inspect and found to carry no treerig identity, so it is not this one.
		for _, other := range listed {
			switch {
			case other.IdentityErr != nil:
				return undetermined(couldNotCheck(other.IdentityErr))
			case other.StatErr != nil:
				return undetermined(couldNotCheck(other.StatErr))
			case other.WorktreeID == "" && other.Root != recordedRoot && !(other.Exists && !other.Prunable):
				return undetermined(hintUnidentified)
			}
		}
		there, err := pathExists(recordedRoot)
		switch {
		case err != nil:
			return undetermined(couldNotCheck(err))
		case there:
			return undetermined(hintDamaged)
		}
		return g.parentEvidence(recordedRoot)
	case entry.IdentityErr != nil:
		return undetermined(couldNotCheck(entry.IdentityErr))
	case entry.StatErr != nil:
		return undetermined(couldNotCheck(entry.StatErr))
	case entry.Exists && entry.Prunable:
		return undetermined(hintDamaged)
	case entry.Exists:
		return evidence{directory: worktree.DirectoryPresent}
	case entry.Locked:
		return undetermined(hintLocked)
	case !entry.Prunable:
		return undetermined(hintNotPrunable)
	}
	return g.parentEvidence(entry.Root)
}

// pathExists reports whether path is there, an error being any failure to tell
// but its absence.
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// parentEvidence judges the absence of path from its parent: the directory is
// gone only if the parent is a real, non-empty directory the mount table does
// not put below an automount placeholder.
func (g *Gatherer) parentEvidence(path string) evidence {
	parent := filepath.Dir(path)
	if parent == path {
		return undetermined(hintParent)
	}
	handle, err := os.Open(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return undetermined(hintParent)
		}
		return undetermined(couldNotCheck(err))
	}
	defer handle.Close()
	if _, err := handle.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return undetermined(hintParent)
		}
		return undetermined(couldNotCheck(err))
	}
	mounts, err := g.mountTable()
	if err != nil {
		return undetermined(couldNotCheck(err))
	}
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return undetermined(hintParent)
		}
		return undetermined(couldNotCheck(err))
	}
	if mounts.Automounted(real) {
		return undetermined(hintParent)
	}
	return evidence{directory: worktree.DirectoryGone}
}
