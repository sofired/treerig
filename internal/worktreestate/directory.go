package worktreestate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sofired/treerig/internal/files"
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

// couldNotCheck is the hint for a check that failed with err: the cause, which
// comes first and without any path (reasonOf), so a long path cannot cut it off.
func couldNotCheck(err error) string {
	return hintCouldNotCheck + text.OneLine(strings.TrimSuffix(reasonOf(err), "."), 200) + "."
}

// GitHint is the hint every worktree of a repository carries when Git could not
// be asked about it (err is why).
func GitHint(err error) string { return couldNotCheck(err) }

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
func (g *Gatherer) directoryOf(p *probes, entry *registry.WorktreeEntry, listed []registry.WorktreeEntry, recordedRoot, slug string, gitErr error) evidence {
	switch {
	case gitErr != nil:
		return undetermined(couldNotCheck(gitErr))
	case entry == nil && recordedRoot == "":
		return undetermined("Git does not list it and no directory was recorded for it: run '" + g.command + " worktree remove " + slug + "' if it is gone.")
	case entry == nil:
		// Git lists nothing with this worktree's identity (pruned, or never in
		// this repository): the same checks, on the directory the last `up`
		// saw it in. But an entry whose identity could not be read, or that
		// could not be examined, or that has none and lives elsewhere without
		// being a live, healthy directory, might be this worktree, moved: then
		// nothing can be said. A live worktree treerig does not manage (a plain `git
		// worktree add`, or the main worktree from before treerig) was looked
		// at by Inspect and found to carry no treerig identity, so it is not
		// this one.
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
		there, err := p.exists(recordedRoot)
		switch {
		case err != nil:
			return undetermined(couldNotCheck(err))
		case there:
			return undetermined(hintDamaged)
		}
		return g.parentEvidence(p, recordedRoot)
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
	return g.parentEvidence(p, entry.Root)
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

// probes is the questions one Gather puts to the file system, all under one
// budget: a hung mount under several worktrees costs the run the probe timeout
// once, not once a worktree, and what is spent is spent. A parent directory
// shared by several worktrees is asked about once.
type probes struct {
	g      *Gatherer
	ctx    context.Context
	parent map[string]parentAnswer
}

// parentAnswer is what looking at one parent directory found.
type parentAnswer struct {
	real string
	err  error
}

// newProbes starts the budget of one Gather; stop releases it.
func (g *Gatherer) newProbes(ctx context.Context) (*probes, context.CancelFunc) {
	budget, stop := context.WithTimeout(ctx, g.probeTimeout)
	return &probes{g: g, ctx: budget, parent: map[string]parentAnswer{}}, stop
}

// exists is pathExists under the budget: a path that does not answer in time
// is an error, never "absent".
func (p *probes) exists(path string) (bool, error) {
	return files.Within(p.ctx, p.g.probeTimeout, func() (bool, error) { return p.g.exists(path) })
}

// realParent is Gatherer.parentOf under the budget, answered once per parent.
func (p *probes) realParent(parent string) (string, error) {
	if known, ok := p.parent[parent]; ok {
		return known.real, known.err
	}
	real, err := files.Within(p.ctx, p.g.probeTimeout, func() (string, error) { return p.g.parentOf(parent) })
	p.parent[parent] = parentAnswer{real, err}
	return real, err
}

// realParent opens parent, reads one name from it and resolves its symbolic
// links, answering the real path. An absent parent is fs.ErrNotExist and an
// empty one io.EOF.
func realParent(parent string) (string, error) {
	handle, err := os.Open(parent)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	if _, err := handle.Readdirnames(1); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(parent)
}

// parentEvidence judges the absence of path from its parent: the directory is
// gone only if the parent is a real, non-empty directory the mount table does
// not put below an automount placeholder. The parent is looked at under the
// run's probe budget, so a mount that does not answer reads as a parent that
// could not be checked.
func (g *Gatherer) parentEvidence(p *probes, path string) evidence {
	parent := filepath.Dir(path)
	if parent == path {
		return undetermined(hintParent)
	}
	real, err := p.realParent(parent)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, io.EOF):
		return undetermined(hintParent)
	case err != nil:
		return undetermined(couldNotCheck(err))
	}
	mounts, err := g.mountTable()
	if err != nil {
		return undetermined(couldNotCheck(err))
	}
	if mounts.Automounted(real) {
		return undetermined(hintParent)
	}
	return evidence{directory: worktree.DirectoryGone}
}

// RecordedDirectory judges the directory a worktree was last seen in by the
// filesystem alone, with the same evidence rule as the listing: it is gone only
// when it is absent and its parent is a real, non-empty directory that is not
// below an automount placeholder. It is DirectoryPresent when it is there, and
// DirectoryUndetermined, with the cause as a sentence, when nothing can be said
// (a path that cannot be checked, a parent that is missing or empty, a mount
// table that cannot be read, the file system not answering within its budget).
// It does not ask Git: `status` uses it for a worktree of another repository,
// where Git is not asked. Its looks at the disk share one budget, as a
// Gather's do, and end with ctx.
func (g *Gatherer) RecordedDirectory(ctx context.Context, path string) (worktree.Directory, string) {
	disk, stop := g.newProbes(ctx)
	defer stop()
	there, err := disk.exists(path)
	switch {
	case err != nil:
		return worktree.DirectoryUndetermined, couldNotCheck(err)
	case there:
		return worktree.DirectoryPresent, ""
	}
	found := g.parentEvidence(disk, path)
	return found.directory, found.hint
}
