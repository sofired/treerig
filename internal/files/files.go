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

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/text"
)

// ControlHomeMarker marks a directory as one the tool created, so it may be
// adopted again later.
const ControlHomeMarker = "." + brand.Name + "-control-home"

// controlHomeDirectories are top-level directories only the tool creates; an
// install from before the marker existed has at least two of them.
var controlHomeDirectories = []string{"repositories", "locks", "router"}

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

// processHomeDir is this process's home directory, read the same way layout.HomeDir does.
func processHomeDir() (string, error) {
	return layout.HomeDir(func(key string) (string, bool) { return os.LookupEnv(key) })
}

// lowerFirst lowercases value's first byte, for splicing a refusal reason
// into the middle of a sentence.
func lowerFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToLower(value[:1]) + value[1:]
}

// AssertAdoptableControlHome checks that controlHome is safe to adopt.
//
// The control home is chmod'd, populated, and — under `worktree remove` —
// recursively deleted within. So it is never a directory the user already
// uses for something else: an existing directory is adopted only when the
// tool created it (the marker, or two of the directories only the tool
// creates, from before the marker existed) or it is empty. The filesystem
// root, the home directory, and anything above it are refused outright.
func AssertAdoptableControlHome(controlHome string) error {
	resolved, err := canonical(controlHome)
	if err != nil {
		return err
	}
	alternatives := fmt.Sprintf(
		"Set %s to an empty directory, or to one %s created earlier (it leaves a %s marker file).",
		brand.HomeEnv, brand.Name, ControlHomeMarker,
	)
	refusal := func(reason string, movable bool) error {
		tail := alternatives
		if movable {
			tail = "Move it aside, or " + lowerFirst(alternatives)
		}
		return clierr.Newf("Refusing to use %s as the control home: %s. %s", resolved, reason, tail)
	}
	if resolved == string(filepath.Separator) {
		return refusal("it is the filesystem root", false)
	}
	homeDir, err := processHomeDir()
	if err != nil {
		return err
	}
	home, err := canonical(homeDir)
	if err != nil {
		return err
	}
	if resolved == home {
		return refusal("it is your home directory", false)
	}
	toHome, err := relativeJS(resolved, home)
	if err != nil {
		return err
	}
	if !(toHome == ".." || strings.HasPrefix(toHome, ".."+string(filepath.Separator)) || filepath.IsAbs(toHome)) {
		return refusal("it contains your home directory", false)
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
	found := 0
	for _, name := range controlHomeDirectories {
		if contains(meaningful, name) {
			found++
		}
	}
	if found >= 2 {
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

// ControlHomeFormat is what the Go build writes into the marker. The
// TypeScript build left it empty, so an empty marker is how a control home it
// created is told apart.
const ControlHomeFormat = "format 2\n"

// LegacyControlHome reports whether the TypeScript build created this control
// home and no Go build has written to it since: its marker is empty, or it
// predates the marker and has at least two of the tool's own directories.
//
// Nothing in this build calls it yet: it is for doctor's legacy-state line,
// which comes with doctor's port. Commands need no such check — the first
// write upgrades the marker, and every lock's leftovers are judged as taken.
func LegacyControlHome(controlHome string) (bool, error) {
	marker := filepath.Join(controlHome, ControlHomeMarker)
	contents, err := os.ReadFile(marker)
	if err == nil {
		return len(contents) == 0, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	entries, err := os.ReadDir(controlHome)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	found := 0
	for _, entry := range entries {
		if contains(controlHomeDirectories, entry.Name()) {
			found++
		}
	}
	return found >= 2, nil
}

// establishControlHome creates the control home itself, after checking it is
// ours to use, and leaves the marker.
func establishControlHome(controlHome string) error {
	if err := AssertAdoptableControlHome(controlHome); err != nil {
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
	dir, err := os.Open(controlHome)
	if err != nil {
		return err
	}
	defer dir.Close()

	marker := filepath.Join(controlHome, ControlHomeMarker)
	contents, err := readMarkerDirect(dir, marker)
	if err != nil {
		return err
	}
	// New, or left empty by the TypeScript build: from now on a Go build has
	// written here. Its lock leftovers are judged lock by lock as they are
	// next taken; its records, cards and markers are read as they are. A
	// marker with anything else in it — a later build's format, say — is not
	// this build's to downgrade, and is left as it is.
	if len(contents) == 0 {
		// A direct write, like the TypeScript build's own touchSecure — not
		// AtomicWrite's temp-then-rename: a concurrent adopter of this same
		// brand new control home would otherwise see that temp file sitting
		// in an empty directory and refuse it as not ours. The marker is nine
		// bytes; a single write(2) of that size does not tear on any
		// filesystem this tool supports.
		return writeMarkerDirect(dir, marker, []byte(ControlHomeFormat), 0o600)
	}
	return nil
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

// writeMarkerDirect creates or truncates the control home's marker in place
// through dir and writes contents, with no temporary file: see
// establishControlHome for why the marker specifically must never be
// written that way. Refused the same way readMarkerDirect is if it is not,
// or could not be made, a regular file — and, because openMarkerNoFollow
// opens non-blocking, a FIFO planted at the marker's name cannot block this
// call on a reader either: with one present the open succeeds and the Stat
// below then refuses it; with none, the open itself fails with ENXIO.
func writeMarkerDirect(dir *os.File, marker string, contents []byte, mode fs.FileMode) error {
	handle, err := openMarkerNoFollow(dir, filepath.Base(marker), unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY, mode)
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

// SecureControlDirectories creates and locks down the control home's
// machine-wide directories.
//
// As of protocol 4, this no longer includes the shared router's directories
// or files (RouterDir and everything under it): those live in the router's
// own machine-wide state directory, resolved by layout.RouterState and
// secured by internal/router (#12, #13). Creating them here would recreate,
// on this control home's very next command, exactly what a control home's
// router migration just removed.
func SecureControlDirectories(l layout.Control) error {
	if err := establishControlHome(l.ControlHome); err != nil {
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

// SecureRegistrationDirectories are the directories a worktree needs before
// it can be registered — no home yet.
func SecureRegistrationDirectories(l layout.Worktree) error {
	if err := establishControlHome(l.ControlHome); err != nil {
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

// SecureWorktreeDirectories are the directories a worktree needs to run.
// Refuses if a previous removal was interrupted, since the quarantine
// directory means state is half-deleted.
func SecureWorktreeDirectories(l layout.Worktree) error {
	if err := SecureControlDirectories(l.Control); err != nil {
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

// AtomicJSON is AtomicWrite of value marshalled the way the TypeScript build
// does: JSON.stringify(value, null, 2) + "\n". Key order follows the Go
// struct's field order, which callers keep matched to the TypeScript schema.
//
// encoding/json already matches JSON.stringify's own string escaping except
// for one case: it always escapes the JavaScript line separators U+2028 and
// U+2029, which JSON.stringify writes raw. text.FixJSONLineSeparators
// corrects exactly that, byte for byte, on the encoded output — a record's
// domain or a card's branch is free-form enough to carry either.
func AtomicJSON(destination string, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	// Encode already appends the trailing "\n", matching `${...}\n` in the TypeScript build.
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
