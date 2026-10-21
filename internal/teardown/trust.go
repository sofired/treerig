package teardown

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sofired/treerig/internal/layout"
)

// InsideWorkTreeError is CheckLocation's refusal: the control home, or its
// teardown directory, lies inside a git work tree.
type InsideWorkTreeError struct {
	// Path is the canonical path found inside a work tree.
	Path string
	// Root is the work tree's top: the directory holding the .git entry.
	Root string
}

// Error describes where the pin store is and what holds it.
func (e *InsideWorkTreeError) Error() string {
	return fmt.Sprintf("%s is inside the git work tree %s", e.Path, e.Root)
}

// CheckLocation refuses a control home whose teardown store lies inside a git
// work tree, which a branch can write to: it could commit its own pin.json and
// an executable named by its sha256, and an environment file in the checkout
// could point TREERIG_CONTROL_HOME there. The same hole as a cosign that
// resolves inside a checkout (docs/design/teardown.md, "Verification").
//
// The control home and the teardown directory are canonicalised, symlinks
// resolved, and each is walked upward for an entry named .git (a file, for a
// linked worktree, or a directory). No git is run.
//
// home is the one exemption, and it must be independent of the environment, so
// it is a type only HomeFromUserDatabase makes: the zero value exempts nothing.
// The walk stops before reaching a directory that is the same file as the home
// (os.SameFile, so another spelling of it, or a case-insensitive volume's, is
// still it), so a dotfiles repository whose work tree is the home directory does
// not condemn ~/.treerig. $HOME, which a .envrc can set to the checkout itself
// and so move the boundary above the checkout's own .git, can never be it. A
// work tree rooted at the real home directory is therefore not guarded.
func CheckLocation(l layout.Control, home HomeBoundary) error {
	boundary := home.info
	for _, dir := range []string{l.ControlHome, l.TeardownDir} {
		resolved := canonicalPath(dir)
		for current := resolved; ; {
			if boundary != nil {
				if info, err := os.Stat(current); err == nil && os.SameFile(info, boundary) {
					break
				}
			}
			if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
				return &InsideWorkTreeError{Path: resolved, Root: current}
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return nil
}

// HomeBoundary is the directory CheckLocation does not look past. Its zero
// value is none: nothing is exempt. The only way to make one is
// HomeFromUserDatabase, so a caller cannot hand CheckLocation a $HOME.
type HomeBoundary struct {
	info os.FileInfo
}

// HomeFromUserDatabase is the boundary for a dotfiles repository at the home
// directory: listed, the home directory the user database lists for the
// effective user (looked up by the entry point, never read from the
// environment), is the boundary only when it exists and is the same file as
// claimed, the home the environment names. An empty or missing listed, a
// nonexistent one, or one that disagrees with claimed gives the zero value,
// which exempts nothing: the environment may be a checkout's, and a database
// answer that disagrees with it is not one to rely on.
func HomeFromUserDatabase(listed, claimed string) HomeBoundary {
	if listed == "" || claimed == "" {
		return HomeBoundary{}
	}
	listedInfo, err := os.Stat(canonicalPath(listed))
	if err != nil {
		return HomeBoundary{}
	}
	claimedInfo, err := os.Stat(canonicalPath(claimed))
	if err != nil || !os.SameFile(listedInfo, claimedInfo) {
		return HomeBoundary{}
	}
	return HomeBoundary{info: listedInfo}
}

// canonicalPath is path with symlinks resolved where it exists, else cleaned.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// UntrustedError is CheckOwnership's refusal: something in the pin store is
// not private to the user running treerig.
type UntrustedError struct {
	// Path is the offending file or directory.
	Path string
	// Problem finishes "<path> ...", such as "is owned by uid 0, not by you
	// (uid 1000)".
	Problem string
}

// Error names the path and what is wrong with it.
func (e *UntrustedError) Error() string { return e.Path + " " + e.Problem }

// euid is the effective user id the ownership check compares with; a variable
// only so a test can stand in for another user.
var euid = os.Geteuid

// CheckOwnership requires that the control home, its teardown directory,
// pin.json and the stored binary (through the descriptor binary holds, the
// file that will run) are owned by the effective user and cannot be written
// by their group or others. A pin store another user can write is a pin
// anyone could rewrite, and the pin's whole value is that only its owner can.
func CheckOwnership(l layout.Control, binary *Binary) error {
	check := func(path string, info os.FileInfo) error {
		if info.Mode()&os.ModeSymlink != 0 {
			return &UntrustedError{Path: path, Problem: "is a symbolic link"}
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != euid() {
			return &UntrustedError{Path: path, Problem: fmt.Sprintf("is owned by uid %d, not by you (uid %d)", stat.Uid, euid())}
		}
		if mode := info.Mode().Perm(); mode&0o022 != 0 {
			return &UntrustedError{Path: path, Problem: fmt.Sprintf("can be written by its group or others (mode %04o)", mode)}
		}
		return nil
	}
	if info, err := os.Stat(l.ControlHome); err != nil {
		return &UntrustedError{Path: l.ControlHome, Problem: fmt.Sprintf("cannot be checked (%s)", reasonOf(err))}
	} else if err := check(l.ControlHome, info); err != nil {
		return err
	}
	for _, path := range []string{l.TeardownDir, l.PinPath} {
		info, err := os.Lstat(path)
		if err != nil {
			return &UntrustedError{Path: path, Problem: fmt.Sprintf("cannot be checked (%s)", reasonOf(err))}
		}
		if err := check(path, info); err != nil {
			return err
		}
	}
	if binary != nil && binary.file != nil {
		info, err := binary.file.Stat()
		if err != nil {
			return &UntrustedError{Path: binary.Path, Problem: fmt.Sprintf("cannot be checked (%s)", reasonOf(err))}
		}
		return check(binary.Path, info)
	}
	return nil
}

// MountNoExec reports whether the filesystem that holds (or would hold) path
// is mounted noexec, so that an exec of the pinned binary would fail. The
// nearest existing ancestor of path is asked, since the control home may not
// exist yet. It reads the mount's flags and runs nothing.
func MountNoExec(path string) (bool, error) {
	for current := filepath.Clean(path); ; {
		if _, err := os.Stat(current); err == nil {
			return mountedNoExec(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, fmt.Errorf("no part of %s exists", path)
		}
		current = parent
	}
}
