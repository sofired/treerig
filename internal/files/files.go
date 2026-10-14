// Package files is the filesystem safety layer every managed path goes
// through: containment (no symlink in the chain, nothing outside the control
// home), secure directory creation, and atomic writes.
//
// Everything under the control home is created owner-only (0700 directories,
// 0600 files) and every path is verified to sit inside the control home with
// no symlink in the chain before it is created or removed. That containment
// is what makes recursive removal during cleanup safe.
//
// The one exception is the two directories the router stack bind-mounts
// (router/index, router/dynamic). Their containers run as root without
// CAP_DAC_OVERRIDE, and on Linux Docker Engine that makes a 0700 directory
// owned by the user unreadable to them. Those two are 0755 with 0644 files.
// Their parents stay 0700, so other local users still cannot reach them on
// disk; only a process handed the directory by a bind mount can. Nothing
// secret is ever written there.
//
// WriteNew is the primitive for creating a file at a path of the user's
// choosing, outside the control home, without ever replacing what is there.
package files

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/text"
)

// ControlHomeMarker marks a directory as one the tool created, so it may be
// adopted again later.
const ControlHomeMarker = "." + brand.Name + "-control-home"

// incidentalEntries are files the operating system drops into directories a
// user merely looked at.
var incidentalEntries = map[string]bool{
	".DS_Store":   true,
	".localized":  true,
	"Thumbs.db":   true,
	"desktop.ini": true,
}

// PathExists reports whether file exists, following symlinks.
func PathExists(file string) (bool, error) {
	if _, err := os.Stat(file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// relativeJS is Node's path.relative: like filepath.Rel, but the "same
// directory" case is "" rather than ".".
func relativeJS(root, candidate string) (string, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", err
	}
	if relative == "." {
		relative = ""
	}
	return relative, nil
}

// AssertSafeDirectoryPath refuses a candidate outside root, and refuses a
// symlink or non-directory anywhere in the chain between them. A candidate
// that does not exist yet is fine, all the way from the first missing
// segment down. Its outside-of-root refusal names root "the control home",
// matching what this tool's every control-home-rooted caller has always
// said; a caller whose root is something else wants
// AssertSafeDirectoryPathNamed instead.
func AssertSafeDirectoryPath(root, candidate string) error {
	return AssertSafeDirectoryPathNamed(root, candidate, "the control home")
}

// AssertSafeDirectoryPathNamed is AssertSafeDirectoryPath with noun in its
// outside-of-root refusal in place of "the control home" — for a root that
// is not one, such as the shared router's own state directory.
func AssertSafeDirectoryPathNamed(root, candidate, noun string) error {
	resolvedRoot, err := layout.Resolve(root)
	if err != nil {
		return err
	}
	resolvedCandidate, err := layout.Resolve(candidate)
	if err != nil {
		return err
	}
	relative, err := relativeJS(resolvedRoot, resolvedCandidate)
	if err != nil {
		return err
	}
	if !(relative == "" || (!strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative))) {
		return clierr.Newf("Refusing directory outside %s: %s", noun, resolvedCandidate)
	}
	var segments []string
	if relative != "" {
		segments = strings.Split(relative, string(filepath.Separator))
	}
	current := resolvedRoot
	for index := -1; index < len(segments); index++ {
		if index >= 0 {
			current = filepath.Join(current, segments[index])
		}
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return clierr.Newf("Refusing symlinked or non-directory managed path: %s", current)
		}
	}
	return nil
}

// AssertSafeFilePath is AssertSafeDirectoryPath for candidate's parent, plus
// a check that candidate itself, if it exists, is a regular non-symlinked
// file. Its outside-of-root refusal names root "the control home"; a caller
// whose root is something else wants AssertSafeFilePathNamed instead.
func AssertSafeFilePath(root, candidate string) error {
	return AssertSafeFilePathNamed(root, candidate, "the control home")
}

// AssertSafeFilePathNamed is AssertSafeFilePath with noun in its
// outside-of-root refusal in place of "the control home".
func AssertSafeFilePathNamed(root, candidate, noun string) error {
	if err := AssertSafeDirectoryPathNamed(root, filepath.Dir(candidate), noun); err != nil {
		return err
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return clierr.Newf("Refusing symlinked or non-file managed path: %s", candidate)
	}
	return nil
}

// createSecureDirectories is deliberately not preceded by a process-wide
// umask change: that would be inherited by every hook the tool spawns,
// making files a hook creates unreadable inside containers. Explicit modes
// plus the chmod below are enough.
func createSecureDirectories(controlHome string, directories []string, mode fs.FileMode) error {
	for _, directory := range directories {
		if err := AssertSafeDirectoryPath(controlHome, directory); err != nil {
			return err
		}
		if err := os.MkdirAll(directory, mode); err != nil {
			return err
		}
		if err := AssertSafeDirectoryPath(controlHome, directory); err != nil {
			return err
		}
		// Unconditional, so an install created before a mode changed is brought up to date.
		if err := os.Chmod(directory, mode); err != nil {
			return err
		}
	}
	return nil
}

// canonical is target with every symlink resolved, falling back to
// layout.Resolve (lexical only) for a target that does not exist yet.
func canonical(target string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, nil
	}
	return layout.Resolve(target)
}

// lowerFirst lowercases value's first byte, for splicing a refusal reason
// into the middle of a sentence.
func lowerFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToLower(value[:1]) + value[1:]
}

// homeRefusalReasonAgainst reports why resolved is unsafe for this tool to
// treat as a directory it owns outright — the filesystem root, userHome, or
// anything above userHome — or "" when none of those apply. Shared by
// AssertAdoptableControlHome (the control home) and AssertSafeHome (the
// berth root, BERTH_HOME), which each phrase the refusal in their own
// words. userHome is always the caller's own resolved home directory (in
// production, layout.HomeDir(getenv) from internal/commands' checkedHome,
// #80) — this package does not read the environment itself for it, only
// the entry point does.
//
// An empty userHome is refused outright here too, the same way
// commands.checkedHome already refuses it before either assertion is ever
// reached in production: canonical("") resolves as the current directory
// (filepath.EvalSymlinks("") succeeds as "."), so comparing resolved
// against it would silently answer a question about the current directory
// rather than about home — or fail deep inside filepath.Rel with "Rel:
// can't make . relative to …", naming neither BERTH_HOME nor
// BERTH_CONTROL_HOME. Any caller that reaches either assertion with an
// empty userHome by some path other than checkedHome gets the same clear
// refusal instead.
func homeRefusalReasonAgainst(resolved, userHome string) (string, error) {
	if userHome == "" {
		return "", clierr.New("Could not determine your home directory; set HOME.")
	}
	if resolved == string(filepath.Separator) {
		return "it is the filesystem root", nil
	}
	home, err := canonical(userHome)
	if err != nil {
		return "", err
	}
	if resolved == home {
		return "it is your home directory", nil
	}
	toHome, err := relativeJS(resolved, home)
	if err != nil {
		return "", err
	}
	if !(toHome == ".." || strings.HasPrefix(toHome, ".."+string(filepath.Separator)) || filepath.IsAbs(toHome)) {
		return "it contains your home directory", nil
	}
	return "", nil
}

// AssertSafeHome refuses to use home as the berth root (BERTH_HOME, #80)
// when it is the filesystem root, userHome, or anything above userHome. The
// berth root holds router/ and, unless BERTH_CONTROL_HOME overrides it, the
// control home — so accepting any of those would put router state or a
// control home inside, or in place of, the directory the user actually
// lives in. userHome is the caller's own resolved home directory (in
// production, layout.HomeDir(getenv) from internal/commands' home.go) —
// this package does not read the environment itself for it, only the
// entry point does.
//
// Unlike AssertAdoptableControlHome, home is never itself chmod'd, emptied,
// or recursively deleted — router.SecureState and AssertAdoptableControlHome
// each check what they create underneath it on their own terms — so this
// applies only this narrower family of refusals, not the emptiness/marker
// check a directory this tool owns outright also needs.
func AssertSafeHome(home, userHome string) error {
	resolved, err := canonical(home)
	if err != nil {
		return err
	}
	reason, err := homeRefusalReasonAgainst(resolved, userHome)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	return clierr.Newf("Refusing to use %s as %s: %s. Point %s elsewhere.", resolved, brand.HomeEnv, reason, brand.HomeEnv)
}

// AssertAdoptableControlHome checks that controlHome is safe to adopt.
// userHome is the caller's own resolved home directory (in production,
// layout.HomeDir(getenv) from internal/commands' checkedHome, #80).
//
// The control home is chmod'd, populated, and — under `worktree remove` —
// recursively deleted within. So it is never a directory the user already
// uses for something else: an existing directory is adopted only when the
// tool created it (the marker) or it is empty. The filesystem root, the
// home directory, and anything above it are refused outright.
func AssertAdoptableControlHome(controlHome, userHome string) error {
	resolved, err := canonical(controlHome)
	if err != nil {
		return err
	}
	alternatives := fmt.Sprintf(
		"Set %s to an empty directory, or to one %s created earlier (it leaves a %s marker file).",
		brand.ControlHomeEnv, brand.Name, ControlHomeMarker,
	)
	refusal := func(reason string, movable bool) error {
		tail := alternatives
		if movable {
			tail = "Move it aside, or " + lowerFirst(alternatives)
		}
		return clierr.Newf("Refusing to use %s as the control home: %s. %s", resolved, reason, tail)
	}
	reason, err := homeRefusalReasonAgainst(resolved, userHome)
	if err != nil {
		return err
	}
	if reason != "" {
		return refusal(reason, false)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var meaningful []string
	for _, entry := range entries {
		if !incidentalEntries[entry.Name()] {
			meaningful = append(meaningful, entry.Name())
		}
	}
	if len(meaningful) == 0 || contains(meaningful, ControlHomeMarker) {
		return nil
	}
	shown := strings.Join(firstN(meaningful, 3), ", ")
	if len(meaningful) > 3 {
		shown += ", …"
	}
	return refusal(fmt.Sprintf("it is not empty (%s) and was not created by %s", shown, brand.Name), true)
}

// contains reports whether target is in values.
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// firstN is the first n values, or all of them if there are n or fewer.
func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

// ControlHomeFormat is what this build writes into the marker: the line
// stateformat.FormatLine(stateformat.ControlHome), pinned equal by
// TestControlHomeFormatVersion. It is a storage name: the marker's bytes are
// on disk in every control home (docs/design/state.md).
//
// Its bytes must never vary by writer (no PID, timestamp or nonce):
// writeMarkerRacingCreators claims a marker still empty after a bounded
// wait, a narrow exception to this project's rule against recovering
// anything by age alone, and that claim is safe only because every writer
// it can race writes these same bytes at offset zero. A different build's
// format landing last is refused by establishControlHome's format check,
// not adopted. The wait is not tuned for slow network filesystems; there it
// only makes the claim path more common, not unsafe.
const ControlHomeFormat = "format 2\n"

// establishControlHome creates the control home itself, after checking it is
// ours to use, and leaves the marker. userHome is the caller's own resolved
// home directory, passed straight through to AssertAdoptableControlHome.
func establishControlHome(controlHome, userHome string) error {
	if err := AssertAdoptableControlHome(controlHome, userHome); err != nil {
		return err
	}
	if err := createSecureDirectories(controlHome, []string{controlHome}, 0o700); err != nil {
		return err
	}

	// Opened once as a directory handle, and used for both the marker's read
	// and its write below, via openat(2): a path-based check followed by a
	// path-based open (the previous shape here) is two separate resolutions
	// of the same name, each its own window for a symlink — or, if the
	// control home's parent is writable by someone else, the control home
	// itself — to be swapped in between. A descriptor already open on the
	// directory is unaffected by what happens to the path afterwards.
	//
	// os.Root is not enough here on its own: per `go doc os.Root`, its
	// methods follow a symlink whose target stays inside the root, and the
	// marker must be refused outright, wherever it points — so it is opened
	// directly against this directory's descriptor instead, with O_NOFOLLOW.
	if beforeOpenControlHomeForTest != nil {
		beforeOpenControlHomeForTest(controlHome)
	}
	dir, err := openControlHomeNoFollow(controlHome)
	if err != nil {
		return err
	}
	defer dir.Close()

	// What else the control home holds is read before the marker, not
	// after: a concurrent creator of this same brand new control home
	// writes its marker in full — via writeMarkerRacingCreators, which
	// never truncates an existing marker (see its own comment) — before it
	// creates anything else here. So anything found now means that
	// creator's marker was already complete, and stays complete: nothing
	// below this point ever truncates a marker that already holds content,
	// so an empty marker read once the directory is otherwise populated is
	// never a creator mid-write, only a leftover this build did not write
	// (refused below).
	populated, err := hasMeaningfulEntries(dir)
	if err != nil {
		return err
	}
	marker := filepath.Join(controlHome, ControlHomeMarker)
	contents, err := readMarkerDirect(dir, marker)
	if err != nil {
		return err
	}
	if afterMarkerReadForTest != nil {
		afterMarkerReadForTest(controlHome)
	}
	if len(contents) == 0 && populated {
		return clierr.Newf(
			"Refusing to use %s as the control home: its marker %s is empty but the directory already holds %s state. Point %s at an empty directory instead.",
			controlHome, marker, brand.Name, brand.ControlHomeEnv)
	}
	if len(contents) == 0 {
		// No marker yet, or one another goroutine (or process, for two
		// berth commands starting at once on a brand new control home)
		// created but has not yet written: writeMarkerRacingCreators
		// either wins the create itself, or waits for whichever creator
		// did and adopts what it wrote — directly rather than through
		// AtomicWrite's temp-then-rename: a concurrent securer of this
		// same brand new control home would otherwise see that temp file
		// sitting in what should be an empty directory and refuse it as
		// not ours. The marker is nine bytes; a single write(2) of that
		// size does not tear on any filesystem this tool supports.
		contents, err = writeMarkerRacingCreators(dir, marker)
		if err != nil {
			return err
		}
	}
	return checkControlHomeMarker(controlHome, marker, contents)
}

// checkControlHomeMarker judges the marker's contents. Exactly this build's
// own format is accepted. A "format N" with N above it is a control home a
// newer berth made: refused with the upgrade instruction (stateformat.Newer),
// never rewritten. Anything else, a smaller number included, is not a marker
// this build wrote: it is foreign, refused the same way as a directory that
// is not a control home at all. Reached both for a marker establishControlHome
// read directly, and for one writeMarkerRacingCreators adopted from whichever
// creator won a race for it, so a foreign winner is refused exactly the same
// way.
func checkControlHomeMarker(controlHome, marker string, contents []byte) error {
	if string(contents) == ControlHomeFormat {
		return nil
	}
	if line, ok := strings.CutSuffix(string(contents), "\n"); ok {
		if verdict, found := stateformat.Line(line, stateformat.ControlHome); verdict == stateformat.FromNewer {
			return stateformat.Newer("use", "the control home "+controlHome, found, stateformat.ControlHome)
		}
	}
	return clierr.Newf(
		"Refusing to use %s as the control home: its marker %s is not %s's own format. Point %s at an empty directory instead.",
		controlHome, marker, brand.Name, brand.ControlHomeEnv)
}

// AssertControlHomeFormat is the read-only form of establishControlHome's
// marker check, for `doctor`: nil when controlHome has no marker yet (nothing
// has claimed it) or one in this build's format, and checkControlHomeMarker's
// refusal otherwise. It never writes, and reads the marker as
// establishControlHome does, through the directory's descriptor and refusing
// a symlink.
func AssertControlHomeFormat(controlHome string) error {
	dir, err := openControlHomeNoFollow(controlHome)
	if err != nil {
		return err
	}
	defer dir.Close()
	marker := filepath.Join(controlHome, ControlHomeMarker)
	contents, err := readMarkerDirect(dir, marker)
	if err != nil {
		return err
	}
	if len(contents) == 0 {
		return nil
	}
	return checkControlHomeMarker(controlHome, marker, contents)
}

// afterMarkerReadForTest runs, if set, in establishControlHome right after
// it has read populated and the marker's own contents, but before it acts on
// what it read — the moment a goroutine (or process) that goes on to lose a
// race to establish this same brand new control home is sitting in, with a
// stale "nothing here yet" view, while a faster one finishes. Tests only;
// nil in production, where establishControlHome's call is a no-op.
var afterMarkerReadForTest func(controlHome string)

// hasMeaningfulEntries reports whether dir holds anything besides the
// control-home marker and the files an operating system drops into any
// directory (incidentalEntries). It reads through dir's own descriptor, so
// it sees the directory establishControlHome already opened, not whatever
// the path names by now.
func hasMeaningfulEntries(dir *os.File) (bool, error) {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return false, err
	}
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	for _, name := range names {
		if name != ControlHomeMarker && !incidentalEntries[name] {
			return true, nil
		}
	}
	return false, nil
}

// beforeOpenControlHomeForTest runs, if set, just after
// createSecureDirectories' own checks and just before establishControlHome
// opens the control home itself — the one window in which a same-parent
// attacker could swap it out from under those checks. Tests only; nil in
// production, where establishControlHome's call is a no-op.
var beforeOpenControlHomeForTest func(controlHome string)

// openControlHomeNoFollow opens controlHome as a directory, with O_NOFOLLOW:
// os.Open would follow a symlink at controlHome's own final path component,
// reintroducing exactly the race the directory handle this returns exists to
// close (see establishControlHome). Refused the same way
// AssertSafeDirectoryPath refuses one in a managed path's chain.
func openControlHomeNoFollow(controlHome string) (*os.File, error) {
	fd, err := unix.Open(controlHome, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, clierr.Newf("Refusing symlinked or non-directory managed path: %s", controlHome)
		}
		return nil, &fs.PathError{Op: "open", Path: controlHome, Err: err}
	}
	return os.NewFile(uintptr(fd), controlHome), nil
}

// markerCreateAttempts bounds writeMarkerRacingCreators' retries of its own
// create-open after the macOS ENOENT quirk described on that function:
// generous headroom for a transient race, not a loop meant to run for long.
const markerCreateAttempts = 10

// markerWaitAttempts and markerWaitInterval bound how long
// writeMarkerRacingCreators polls an existing, still-empty marker for
// whichever goroutine or process created it to write it, once this call's
// own create loses that race with EEXIST. The winner's write is a single
// nine-byte write(2) immediately following its create (see
// writeMarkerRacingCreators), so markerWaitAttempts*markerWaitInterval only
// needs to survive scheduling jitter, not real work.
const (
	markerWaitAttempts = 50
	markerWaitInterval = 2 * time.Millisecond
)

// writeMarkerRacingCreators writes the control home's marker, tolerating
// another goroutine's — or, for two berth commands starting at once on a
// brand new control home, another process's — own concurrent create of the
// same name, and returns whatever content the marker ends up holding: this
// call's own write, or a winner's, adopted rather than overwritten.
//
// It creates with O_EXCL and never truncates an existing marker. An earlier
// version created with O_CREAT|O_TRUNC, which let a goroutine that lost the
// race truncate a winner's already-complete marker back to empty for the
// width of its own create-then-write; a third goroutine reading in that
// window saw an empty marker beside a directory a winner had already
// populated, and refused a control home that was never actually broken.
//
// On EEXIST, another creator's own create already won: this call waits,
// polling markerWaitAttempts times over markerWaitInterval each, for that
// creator's content to appear. If it appears, that content is returned,
// unread — establishControlHome's own format check runs on it just the same
// as on content this call read directly, so a foreign winner's marker is
// still refused rather than adopted. If markerWaitAttempts polls all still
// find it empty, no live creator is in flight: a live creator's write always
// immediately follows its create (the invariant this function itself
// upholds), so a marker that stays empty for that whole bounded wait is a
// leftover from one that crashed between its create and its write, not a
// slow one still to catch up. This call then claims it in place — open
// without O_EXCL or O_TRUNC, so an existing file is neither refused nor
// emptied — which is safe even if another goroutine reaches the same
// conclusion and does the same at the same time: every claimant writes the
// same nine bytes at offset zero, so whichever writes last leaves the
// correct content either way.
//
// macOS CI (run 35984111281, "Tests without cgo, as released") showed the
// create-open fail with ENOENT while 20 goroutines raced to create this same
// marker in TestConcurrentControlHomeEstablishment, intermittently, though
// the same test had passed on macOS before. createSecureDirectories never
// replaces the control home directory itself, so nothing here removes the
// marker's name out from under an O_CREAT open; this comment does not claim
// to know what in the kernel produced that ENOENT, only that it was
// observed. If the create-open fails with ENOENT, the marker is read again:
// another goroutine may have already written it, in which case there is
// nothing left to do; otherwise the create is retried, up to
// markerCreateAttempts times.
func writeMarkerRacingCreators(dir *os.File, marker string) ([]byte, error) {
	contents := []byte(ControlHomeFormat)
	var lastErr error
	for attempt := 0; attempt < markerCreateAttempts; attempt++ {
		err := writeMarkerDirect(dir, marker, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, contents, 0o600)
		if err == nil {
			return contents, nil
		}
		if errors.Is(err, fs.ErrExist) {
			won, waitErr := waitForMarkerWrite(dir, marker)
			if waitErr != nil {
				return nil, waitErr
			}
			if won != nil {
				return won, nil
			}
			if err := writeMarkerDirect(dir, marker, unix.O_CREAT|unix.O_WRONLY, contents, 0o600); err != nil {
				return nil, err
			}
			return contents, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		lastErr = err
		read, readErr := readMarkerDirect(dir, marker)
		if readErr != nil {
			return nil, readErr
		}
		if len(read) != 0 {
			// Another goroutine's own create-and-write won the race; this one's marker is already in place.
			return read, nil
		}
	}
	return nil, clierr.Newf("Could not create the control home marker at %s after %d attempts: %s",
		marker, markerCreateAttempts, lastErr)
}

// waitForMarkerWrite polls the marker through dir, for up to
// markerWaitAttempts times over markerWaitInterval each, for a creator that
// won the race to create it (see writeMarkerRacingCreators) to write it. It
// returns that creator's content once the marker is no longer empty, or nil
// once every poll still found it empty.
func waitForMarkerWrite(dir *os.File, marker string) ([]byte, error) {
	for attempt := 0; attempt < markerWaitAttempts; attempt++ {
		contents, err := readMarkerDirect(dir, marker)
		if err != nil {
			return nil, err
		}
		if len(contents) != 0 {
			return contents, nil
		}
		time.Sleep(markerWaitInterval)
	}
	return nil, nil
}

// openMarkerNoFollow opens name (the marker's own base name) through dir's
// descriptor via openat(2), with O_NOFOLLOW: a symlink at that name is
// refused rather than followed, wherever it points, unlike os.Root's own
// symlink handling (see establishControlHome). O_NONBLOCK is set for the
// open call too, so a FIFO planted at that name cannot block it — a blocking
// O_RDONLY open waits for a writer, and O_WRONLY for a reader, either of
// which would otherwise hang this call forever; opened non-blocking, a read
// open of a FIFO returns at once and a write open with no reader fails with
// ENXIO. The descriptor is put back into blocking mode before it is handed
// back, so a regular file — the only thing ever actually read or written
// here — is read and written exactly as any other file in this codebase.
func openMarkerNoFollow(dir *os.File, name string, flag int, mode fs.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(mode))
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// refusedMarker is the same wording AssertSafeFilePath uses for a symlinked
// or non-regular managed path, for a marker refused after opening rather
// than before.
func refusedMarker(marker string) error {
	return clierr.Newf("Refusing symlinked or non-file managed path: %s", marker)
}

// readMarkerDirect reads the control home's marker through dir: no error and
// no content for a marker that does not exist yet (a brand new control
// home), and a refusal for anything opened that turns out not to be a
// regular file. O_NOFOLLOW already refuses a symlink at open; the Stat below
// catches a device planted at the same path, and — because
// openMarkerNoFollow opens non-blocking — a FIFO too, opened at once rather
// than left blocking this call on a writer that may never come.
func readMarkerDirect(dir *os.File, marker string) ([]byte, error) {
	handle, err := openMarkerNoFollow(dir, filepath.Base(marker), unix.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, refusedMarker(marker)
		}
		return nil, &fs.PathError{Op: "open", Path: marker, Err: err}
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, refusedMarker(marker)
	}
	return io.ReadAll(handle)
}

// writeMarkerDirect opens the control home's marker through dir with flag —
// always including unix.O_WRONLY, and unix.O_CREAT with or without
// unix.O_EXCL depending on the caller (see writeMarkerRacingCreators) — and
// writes contents, with no temporary file: see establishControlHome for why
// the marker specifically must never be written that way. flag never
// includes unix.O_TRUNC: this call must never shorten a marker that already
// holds another creator's complete content down to nothing, even for the
// width of a single syscall (the race writeMarkerRacingCreators' own
// comment describes), so an existing marker is either refused outright by
// unix.O_EXCL or opened and overwritten in place at offset zero, never
// truncated first. Refused the same way readMarkerDirect is if what is
// opened is not, or could not be made, a regular file — and, because
// openMarkerNoFollow opens non-blocking, a FIFO planted at the marker's name
// cannot block this call on a reader either: with one present the open
// succeeds and the Stat below then refuses it; with none, the open itself
// fails with ENXIO.
func writeMarkerDirect(dir *os.File, marker string, flag int, contents []byte, mode fs.FileMode) error {
	handle, err := openMarkerNoFollow(dir, filepath.Base(marker), flag, mode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return refusedMarker(marker)
		}
		return &fs.PathError{Op: "open", Path: marker, Err: err}
	}
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		handle.Close()
		return refusedMarker(marker)
	}
	_, writeErr := handle.Write(contents)
	if chmodErr := handle.Chmod(mode); writeErr == nil {
		writeErr = chmodErr
	}
	if closeErr := handle.Close(); writeErr == nil {
		writeErr = closeErr
	}
	return writeErr
}

// SecureControlDirectories creates and locks down the control home's own
// directories.
//
// As of protocol 4, this no longer includes the shared router's directories
// or files (RouterDir and everything under it): the router's own state and
// lock live under the berth root instead, one copy per berth root — not one
// per control home — resolved by layout.RouterState and secured by
// internal/router (#12, #13, #80); the Traefik stack they describe is
// itself one per Docker daemon, not per berth root or control home.
func SecureControlDirectories(l layout.Control, userHome string) error {
	if err := establishControlHome(l.ControlHome, userHome); err != nil {
		return err
	}
	if err := createSecureDirectories(l.ControlHome, []string{
		l.ControlHome,
		l.RepositoriesDir,
		l.LocksDir,
	}, 0o700); err != nil {
		return err
	}
	return AssertSafeFilePath(l.ControlHome, l.RegistryLockPath)
}

// SecureBinDirectory is the content-addressed store `worktree remove`
// installs a copy of itself into before running (#7), owner-only like every
// other control-home directory. Callers create it lazily, only when they are
// about to install into it, rather than on every command the way
// SecureControlDirectories's own directories are. userHome is passed
// straight through to SecureControlDirectories, for the same
// AssertAdoptableControlHome check every other control-home directory goes
// through (#80).
func SecureBinDirectory(l layout.Control, userHome string) error {
	if err := SecureControlDirectories(l, userHome); err != nil {
		return err
	}
	return createSecureDirectories(l.ControlHome, []string{l.BinDir}, 0o700)
}

// SecureTeardownDirectory is the directory that holds the teardown pin and
// the binaries it names (#85), owner-only like SecureBinDirectory's, and
// created lazily the same way: only a caller about to write the pin needs it.
// userHome goes straight through to SecureControlDirectories. The pin's
// directory is separate from the bin directory on purpose
// (docs/design/teardown.md): that one is swept by age and count.
func SecureTeardownDirectory(l layout.Control, userHome string) error {
	if err := SecureControlDirectories(l, userHome); err != nil {
		return err
	}
	return createSecureDirectories(l.ControlHome, []string{l.TeardownDir}, 0o700)
}

// SecureRegistrationDirectories are the directories a worktree needs before
// it can be registered — no home yet.
func SecureRegistrationDirectories(l layout.Worktree, userHome string) error {
	if err := establishControlHome(l.ControlHome, userHome); err != nil {
		return err
	}
	if err := createSecureDirectories(l.ControlHome, []string{
		l.RepositoriesDir,
		l.LocksDir,
		l.RepositoryDir,
		l.RegistryDir,
		filepath.Dir(l.WorktreeLocksDir),
		l.WorktreeLocksDir,
	}, 0o700); err != nil {
		return err
	}
	if err := AssertSafeFilePath(l.ControlHome, l.RegistryLockPath); err != nil {
		return err
	}
	if err := AssertSafeFilePath(l.ControlHome, l.LockPath); err != nil {
		return err
	}
	return AssertSafeFilePath(l.ControlHome, l.DevelopmentLockPath)
}

// SecureSharedDirectories are the directories a repository's shared services
// need before anything is written for them: the records and override
// directory, and the directory the shared lock lives in. Owner-only like the
// rest of the control home, and checked the same way against a symlinked or
// foreign one.
func SecureSharedDirectories(l layout.Shared, userHome string) error {
	if err := establishControlHome(l.ControlHome, userHome); err != nil {
		return err
	}
	if err := createSecureDirectories(l.ControlHome, []string{
		l.RepositoriesDir,
		l.LocksDir,
		l.RepositoryDir,
		l.Dir,
		l.LockDir,
	}, 0o700); err != nil {
		return err
	}
	if err := AssertSafeFilePath(l.ControlHome, l.RegistryLockPath); err != nil {
		return err
	}
	return AssertSafeFilePath(l.ControlHome, l.LockPath)
}

// SecureWorktreeDirectories are the directories a worktree needs to run.
// Refuses if a previous removal was interrupted, since the quarantine
// directory means state is half-deleted.
func SecureWorktreeDirectories(l layout.Worktree, userHome string) error {
	if err := SecureControlDirectories(l.Control, userHome); err != nil {
		return err
	}
	interrupted, err := PathExists(l.QuarantinePath)
	if err != nil {
		return err
	}
	if interrupted {
		return clierr.New("Removal of this worktree's environment was interrupted; retry the removal before starting it again.")
	}
	return createSecureDirectories(l.ControlHome, []string{
		l.RepositoryDir,
		l.RegistryDir,
		filepath.Dir(l.WorktreeLocksDir),
		l.WorktreeLocksDir,
		l.InstancesDir,
		l.Home,
	}, 0o700)
}

// randomSuffix is eight random hex characters, for a temp file name unique
// across goroutines that share this process's PID.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unheard of on any platform this tool
		// supports; the PID alone is still better than nothing.
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// AtomicWrite writes via a temp file and rename, then fsyncs the directory.
//
// The temp name carries a random suffix, not just this process's PID: two
// goroutines in the one process (never two processes, which always have
// distinct PIDs) can otherwise race to the same temp path and have one's
// rename fail with ENOENT once the other's has already moved it away.
func AtomicWrite(destination string, contents []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.tmp.%d.%s", destination, os.Getpid(), randomSuffix())
	handle, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	writeErr := func() error {
		if _, err := handle.Write(contents); err != nil {
			return err
		}
		if err := handle.Chmod(mode); err != nil {
			return err
		}
		return handle.Sync()
	}()
	if closeErr := handle.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// AtomicJSON is AtomicWrite of value marshalled two-space indented, with a
// trailing newline. Key order follows the Go struct's field order.
//
// encoding/json escapes the JavaScript line separators U+2028 and U+2029 by
// default; text.FixJSONLineSeparators reverses exactly that, byte for byte,
// on the encoded output, so they appear raw — a record's domain or a card's
// branch is free-form enough to carry either, and a raw encoding is easier
// to read and diff on disk.
func AtomicJSON(destination string, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	// Encode already appends the trailing newline.
	return AtomicWrite(destination, text.FixJSONLineSeparators(buffer.Bytes()), 0o600)
}

// RemoveWithin is a recursive removal that refuses anything not strictly
// inside root.
func RemoveWithin(root, target string) error {
	resolvedRoot, err := layout.Resolve(root)
	if err != nil {
		return err
	}
	resolvedTarget, err := layout.Resolve(target)
	if err != nil {
		return err
	}
	relative, err := relativeJS(resolvedRoot, resolvedTarget)
	if err != nil {
		return err
	}
	if relative == "" || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return clierr.Newf("Refusing to remove unsafe path: %s", target)
	}
	return os.RemoveAll(target)
}
