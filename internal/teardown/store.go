package teardown

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/stateformat"
)

// SweepGrace is how recently a stored binary must have been modified for the
// sweep to leave it alone: a concurrent writer's fresh install, which its own
// pin.json rename has not reached yet (docs/design/teardown.md).
const SweepGrace = 10 * time.Minute

// binaryMode is the mode of a stored teardown binary.
const binaryMode fs.FileMode = 0o700

// ReadPin is the one reader of the teardown pin, for `teardown pin`, `teardown
// status` and the removal gate. It returns the pin, or found false when there
// is none. It takes no lock: the file is only ever replaced by an atomic
// rename.
//
// The version is judged first and on its own. A newer one is refused with
// stateformat.Newer (errors.Is(err, stateformat.ErrNewer)); no version, an
// unknown version, an unknown key or a bad field is refused with the foreign
// text (errors.Is(err, ErrForeign)); neither is ever rewritten. Any other
// error is an I/O failure, or a path that is not a regular, non-symlinked file
// in a safe directory.
func ReadPin(l layout.Control) (pin Pin, found bool, err error) {
	if err := files.AssertSafeFilePath(l.ControlHome, l.PinPath); err != nil {
		return Pin{}, false, err
	}
	data, err := os.ReadFile(l.PinPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Pin{}, false, nil
		}
		reason := err
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			reason = pathErr.Err // the path is named once, in the message
		}
		return Pin{}, false, clierr.Wrap(fmt.Sprintf("Could not read the teardown pin %s: %s. Check its permissions, or delete %s/ and pin again.", l.PinPath, reason, l.TeardownDir), err)
	}
	verdict, newer := stateformat.JSON(data, stateformat.TeardownPin)
	switch verdict {
	case stateformat.FromNewer:
		return Pin{}, false, stateformat.Newer("use", "the teardown pin "+l.PinPath, newer, stateformat.TeardownPin)
	case stateformat.Current:
		parsed, err := parse(data)
		if err != nil {
			return Pin{}, false, foreign(l.PinPath, l.TeardownDir, err)
		}
		return parsed, true, nil
	}
	return Pin{}, false, foreign(l.PinPath, l.TeardownDir, nil)
}

// BinaryPath is where the stored binary for hash lives, installed or not.
func BinaryPath(l layout.Control, hash string) string { return binstore.Path(l.TeardownDir, hash) }

// WritePin replaces pin.json with pin by atomic rename, so a reader sees the
// old pin or the new one, never a mixture; the last writer wins. It sets the
// version and refuses a pin that does not validate. It does not install a
// binary or sweep; Save is the whole writer.
func WritePin(l layout.Control, pin Pin) error {
	pin.Version = stateformat.TeardownPin
	if err := pin.Validate(); err != nil {
		return fmt.Errorf("refusing to write an invalid teardown pin: %w", err)
	}
	if err := files.AssertSafeFilePath(l.ControlHome, l.PinPath); err != nil {
		return err
	}
	return files.AtomicJSON(l.PinPath, pin)
}

// Saved is what Save did beyond the pin itself.
type Saved struct {
	// BinaryPath is where binary was installed.
	BinaryPath string
	// Removed are the stored files the sweep removed.
	Removed []string
	// SweepErr is the sweep's first error. The sweep is best effort: the pin
	// is written whatever it says.
	SweepErr error
}

// afterWritePinForTest, if set, runs in Save just after the pin.json rename
// and before the check that the binary is still there. Tests only.
var afterWritePinForTest func(binaryPath string)

// beforeRemoveForTest, if set, runs in Sweep just before the last check of a
// file it is about to remove. Tests only.
var beforeRemoveForTest func(path string)

// Save is the writer. In order, with no lock:
//
//  1. install binary under its sha256 (binstore.Install, atomic and
//     idempotent);
//  2. replace pin.json by atomic rename;
//  3. check that the binary is still there and install it again if not;
//  4. drop the name of any merged-in binary whose file is gone;
//  5. sweep, best effort.
//
// The binary a pin names is on disk before the pin is, which keeps every
// reader's view whole. pin must name binary's hash in one of its platforms, so
// a pin never points at bytes that were not installed. now stamps a reused
// binary and is the sweep's clock.
//
// A sweep by another writer can remove the binary between steps 1 and 2 only
// if the file is already older than SweepGrace by that sweep's clock, which on
// one machine means this writer stalled for more than SweepGrace. Step 3 covers
// a sweep that lands before it. What is left is a window between the sweep's
// last check and its unlink, or a stall that long: the pin then names a
// binary that is not there. The gate checks the binary's hash at every use and
// refuses a missing one (docs/design/teardown.md), so the cost is a refusal
// and a repeated `teardown pin`, never a wrong binary run.
//
// The step-4 rewrite is a pin write like step 2, so it is last-writer-wins too:
// it can replace a pin another writer renamed in between.
//
// The errors Save returns are internal (lowercase, and plain except for the
// path and format refusals): a command wraps them with the context a user
// needs.
func Save(l layout.Control, userHome string, binary []byte, pin Pin, now time.Time) (Saved, error) {
	hash := binstore.Hash(binary)
	named := false
	for _, h := range pin.BinaryHashes() {
		named = named || h == hash
	}
	if !named {
		return Saved{}, fmt.Errorf("the teardown pin does not name the binary being installed (sha256 %s)", hash)
	}
	pin.Version = stateformat.TeardownPin
	if err := pin.Validate(); err != nil {
		return Saved{}, fmt.Errorf("refusing to write an invalid teardown pin: %w", err)
	}
	if err := files.SecureTeardownDirectory(l, userHome); err != nil {
		return Saved{}, err
	}
	// Both paths are checked before anything is written, so a refusal leaves
	// nothing behind.
	for _, path := range []string{BinaryPath(l, hash), l.PinPath} {
		if err := files.AssertSafeFilePath(l.ControlHome, path); err != nil {
			return Saved{}, err
		}
	}
	installed, err := binstore.Install(l.TeardownDir, binary, binaryMode, now)
	if err != nil {
		return Saved{}, fmt.Errorf("installing the teardown binary: %w", err)
	}
	if err := WritePin(l, pin); err != nil {
		return Saved{}, err
	}
	if afterWritePinForTest != nil {
		afterWritePinForTest(installed)
	}
	if _, err := os.Lstat(installed); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return Saved{}, fmt.Errorf("checking the teardown binary: %w", err)
		}
		if installed, err = binstore.Install(l.TeardownDir, binary, binaryMode, now); err != nil {
			return Saved{}, fmt.Errorf("installing the teardown binary again: %w", err)
		}
	}
	// A pin merged from an older one names binaries this writer did not
	// install. One that is gone is a name the gate would refuse; say instead
	// that the platform has none, which `teardown status` reports and pinning
	// again on that machine repairs.
	if fixed, changed := withoutMissingBinaries(l, pin, hash); changed {
		pin = fixed
		if err := WritePin(l, pin); err != nil {
			return Saved{}, err
		}
	}
	removed, sweepErr := Sweep(l, pin, now)
	return Saved{BinaryPath: installed, Removed: removed, SweepErr: sweepErr}, nil
}

// withoutMissingBinaries is pin with binary_sha256 cleared from every platform
// entry whose stored file is gone, except the one named keep (the binary being
// installed), and whether it changed anything. pin is not modified.
func withoutMissingBinaries(l layout.Control, pin Pin, keep string) (Pin, bool) {
	changed := false
	platforms := make(map[string]Platform, len(pin.Platforms))
	for key, entry := range pin.Platforms {
		if entry.BinarySHA256 != "" && entry.BinarySHA256 != keep {
			if _, err := os.Lstat(BinaryPath(l, entry.BinarySHA256)); errors.Is(err, fs.ErrNotExist) {
				entry.BinarySHA256 = ""
				changed = true
			}
		}
		platforms[key] = entry
	}
	pin.Platforms = platforms
	return pin, changed
}

var (
	// storedName is a stored binary's file name: 64 lowercase hex characters.
	storedName = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// leftoverName is what files.AtomicWrite's temporary files are called
	// ("<destination>.tmp.<pid>.<suffix>") for a stored binary or for pin.json,
	// left behind by a writer that was killed between creating and renaming.
	leftoverName = regexp.MustCompile(`^([0-9a-f]{64}|pin\.json)\.tmp\.[0-9]+\.[0-9a-z]+$`)
)

// Sweep removes what a pin does not need from the teardown directory: stored
// binaries it does not name, and the temporary files of killed writers
// (<64-hex>.tmp.<pid>.<suffix> and pin.json.tmp.<pid>.<suffix>). It considers
// only regular files, by Lstat; symlinks, pin.json, directories and everything
// else are left alone.
//
// It reads the pin on disk once, before it removes anything, and keeps every
// binary_sha256 that pin names as well as those the pin argument names. If that
// read fails for any reason but the pin file being absent (an I/O error, a
// foreign or a newer pin, an unsafe path), it removes nothing and returns the
// error: a sweep that cannot tell what is needed deletes nothing. It also keeps
// anything modified within SweepGrace of now, and looks at each file again just
// before removing it, skipping one that is no longer a regular file or has
// become fresh.
//
// It is best effort: a file that cannot be removed does not stop it, and the
// first such error is returned after the rest were tried. It is called by Save
// after the pin.json rename, never by the removal gate or by status, and it is
// not binstore.Prune, which ranks a directory by age and count and has no
// notion of a pin. A pin written after the read can still be left naming a
// binary removed just after it; see Save.
func Sweep(l layout.Control, pin Pin, now time.Time) (removed []string, err error) {
	if err := files.AssertSafeDirectoryPath(l.ControlHome, l.TeardownDir); err != nil {
		return nil, err
	}
	onDisk, _, err := ReadPin(l)
	if err != nil {
		return nil, fmt.Errorf("not sweeping, the pin on disk could not be read: %w", err)
	}
	entries, err := os.ReadDir(l.TeardownDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	named := map[string]bool{}
	for _, hash := range append(pin.BinaryHashes(), onDisk.BinaryHashes()...) {
		named[hash] = true
	}
	var first error
	note := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !(storedName.MatchString(name) || leftoverName.MatchString(name)) || named[name] {
			continue
		}
		path := filepath.Join(l.TeardownDir, name)
		removable, err := removableFile(path, now)
		note(err)
		if !removable {
			continue
		}
		if beforeRemoveForTest != nil {
			beforeRemoveForTest(path)
		}
		removable, err = removableFile(path, now)
		note(err)
		if !removable {
			continue
		}
		if err := os.Remove(path); err != nil {
			note(err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, first
}

// removableFile reports whether path is a regular file (by Lstat) outside the
// grace window. A file that is gone is not removable and not an error.
func removableFile(path string, now time.Time) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular() && now.Sub(info.ModTime()) >= SweepGrace, nil
}
