// Package binstore is a content-addressed store of executable files: install
// once per distinct sha256, install is a no-op for anything already there
// (its own last-used time is refreshed instead), and an old or surplus entry
// is swept away by Prune. `worktree remove` uses one, under
// layout.Control.BinDir, to run itself from a copy that sits outside any
// checkout (#7) — see internal/commands's relocateAndReexec — but nothing
// here knows that; a store just holds bytes under their own hash, whoever's
// bytes they are. It does not vet or trust what it is given: an entry is
// exactly the bytes that were installed, addressed by their own hash,
// nothing more.
package binstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sofired/treerig/internal/files"
)

// Hash is content's sha256, hex-encoded — the name an entry is stored under.
func Hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// HashReader is the hex sha256 of everything r yields, read as a stream.
func HashReader(r io.Reader) (string, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// HashFile is the hex sha256 of the file at path, read as a stream; it
// follows a symlink at path.
func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	return HashReader(file)
}

// Path is where the entry for hash lives under dir, installed or not.
func Path(dir, hash string) string { return filepath.Join(dir, hash) }

// beforeChtimesForTest runs, if set, just after Install confirms an entry
// already exists and just before it calls Chtimes on it — the window a
// concurrent Prune, in another process, can win by removing that entry
// first (Prune's own grace window makes this rare — an entry Install is
// about to revive was, from Prune's point of view, still last touched
// whenever it was originally installed — but does not make it impossible).
// Tests only; nil in production, where Install's own call is a no-op.
var beforeChtimesForTest func(target string)

// Install puts content in dir under its own content address and reports the
// path, atomically (files.AtomicWrite: temp file, then rename) and
// idempotently. An entry already there for this hash is reused only once
// it is checked: a regular file with exactly mode's permissions and exactly
// content's bytes. Anything else under that name — corrupted, replaced, or
// left with other permissions — is replaced atomically, so the path this
// returns always holds what the caller asked for. A reused entry is not
// rewritten, but its modification time is refreshed to now, so Prune's
// age, its grace window, and its eviction order all read as time since
// last use, not merely time since the entry was first written. If the
// entry vanishes between this check and that refresh — a concurrent
// Prune, in another process, racing this same revival and winning — it is
// written fresh instead, the same as if it had never been there: the
// bytes this call was given are still exactly what belongs at that path,
// so there is nothing to treat as a failure. Two processes installing the
// same content at once both write their own uniquely-named temp file and
// rename over the same destination; since the bytes are identical either
// way, whichever rename lands last is harmless.
func Install(dir string, content []byte, mode fs.FileMode, now time.Time) (string, error) {
	target := Path(dir, Hash(content))
	exists, err := files.PathExists(target)
	if err != nil {
		return "", err
	}
	if exists && !matches(target, content, mode) {
		exists = false // replace it below, atomically
	}
	if exists {
		if beforeChtimesForTest != nil {
			beforeChtimesForTest(target)
		}
		switch err := os.Chtimes(target, now, now); {
		case err == nil:
			return target, nil
		case !os.IsNotExist(err):
			return "", err
		}
		// os.IsNotExist(err): raced away by a concurrent Prune between the
		// check above and here; fall through and write it fresh.
	}
	if err := files.AtomicWrite(target, content, mode); err != nil {
		return "", err
	}
	return target, nil
}

// matches reports whether target is a regular file with exactly mode's
// permissions and exactly content's bytes. Any error reading it counts as
// a mismatch: Install then writes it fresh, and a real problem with the
// directory surfaces from that write instead.
func matches(target string, content []byte, mode fs.FileMode) bool {
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() || info.Size() != int64(len(content)) {
		return false
	}
	existing, err := os.ReadFile(target)
	return err == nil && bytes.Equal(existing, content)
}

// Prune keeps the maxEntries most recently used; the rest go, oldest-used
// first. Precisely: an entry directly under dir — except keep, never a
// candidate — is removed when both of these hold: its modification time is
// at least grace old (relative to now), and it is either at least
// olderThan old, or, once every entry that old is set aside, ranked past
// the first maxEntries of what is left, oldest-used first. grace exists
// for entries the sweep cannot tell apart from one another process is
// installing or about to exec into right now — Install refreshes an
// entry's modification time before either of those, so anything younger
// than grace is presumed to be exactly that, regardless of what age or
// rank alone would say. This is disk hygiene for a build cache, not a
// safety property: nothing here proves an entry unused before removing
// it, only that it looks old and unranked enough, and grace narrows the
// one race that matters (removing what another process just installed or
// is about to run) to a sub-millisecond gap it cannot close outright —
// between this sweep reading an entry's modification time as old enough to
// remove and the os.Remove call moments later, another process's own
// Install can still revive that exact entry, and lose it anyway; the
// loser of that race falls back to running whatever binary it already
// has, never to a correctness failure (see internal/commands's
// relocateAndReexec).
//
// Best-effort otherwise: one entry that cannot be removed does not stop
// the sweep, though its error is still reported once the sweep finishes,
// the first one encountered, in the deterministic order below. A target
// that vanishes mid-sweep (removed by another process's own prune, or by
// hand) is not an error: POSIX unlink-while-open means a process still
// executing a pruned entry keeps running regardless.
func Prune(dir, keep string, olderThan time.Duration, maxEntries int, grace time.Duration, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type candidate struct {
		path  string
		mtime time.Time
	}
	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// toRemove is built in a fixed order (ReadDir's own order for the age
	// rule, then oldest-used-first for the count rule) rather than
	// gathered into a map, so which entry's error is reported first, when
	// more than one fails, does not depend on Go's randomized map order.
	var toRemove []string
	var live []candidate
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == keep {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if !os.IsNotExist(err) {
				record(err)
			}
			continue
		}
		age := now.Sub(info.ModTime())
		if age < grace {
			continue
		}
		if age >= olderThan {
			toRemove = append(toRemove, path)
			continue
		}
		live = append(live, candidate{path, info.ModTime()})
	}
	// Oldest-used first, so a cap past maxEntries evicts what has sat
	// unused longest among what age alone would still keep. Stable so two
	// entries revived at exactly the same instant (Install's own now,
	// shared by an install and a rename inside the same call) keep
	// whatever order ReadDir gave them, rather than an arbitrary one.
	sort.SliceStable(live, func(i, j int) bool { return live[i].mtime.Before(live[j].mtime) })
	if excess := len(live) - maxEntries; excess > 0 {
		for _, c := range live[:excess] {
			toRemove = append(toRemove, c.path)
		}
	}

	for _, path := range toRemove {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			record(err)
		}
	}
	return firstErr
}
