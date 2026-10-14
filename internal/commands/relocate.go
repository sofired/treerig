package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// standaloneMaxAge is how long an entry in the standalone-binary store may
// sit unused before relocateAndReexec sweeps it away. Generous on purpose:
// this is disk hygiene for a build cache, not a correctness property, and a
// worktree torn down only occasionally should not lose its entry between
// removals.
const standaloneMaxAge = 30 * 24 * time.Hour

// standaloneMaxEntries bounds the standalone-binary store by count, not
// only by age: the entry just installed or used aside, at most this many
// others are kept, oldest-used evicted first. Without this, a workflow that
// rebuilds often — `go run`, or a WIP branch rebuilt on every `worktree
// remove` — would grow the store by one entry per distinct build forever,
// since each is younger than standaloneMaxAge for a month regardless of how
// many there are.
//
// A steady-state target, not a hard ceiling: it is enforced lazily, only by
// the sweep a later `worktree remove` runs, and standaloneGrace exempts
// whatever was installed or used in the last ten minutes from it — so a
// burst of removals from distinct builds in quick succession can leave the
// store above this count for a while, until a subsequent removal's own
// sweep, past everyone's grace window, brings it back down.
const standaloneMaxEntries = 5

// standaloneGrace is how recently an entry must have been installed or
// revived (binstore.Install's own modification-time refresh) for
// binstore.Prune to leave it alone regardless of age or rank. It exists for
// two other processes' own relocateAndReexec running at the same time: one
// mid-way through installing or about to exec into an entry this process's
// own sweep would otherwise be free to remove out from under it. Ten
// minutes is far longer than either ever takes; see binstore.Prune's own
// doc comment for the narrow race even this cannot close.
const standaloneGrace = 10 * time.Minute

// relocateAndReexec is `worktree remove`'s first act (issue #7). What it
// guarantees, exactly: it installs a copy of the running binary into the
// content-addressed store under the control home — outside any checkout,
// by construction — and, unless this process is already running from that
// store, re-execs into the installed copy with the same argv and
// environment, so the destructive verb that follows runs from a file the
// checkout it might be deleting cannot take down with it.
//
// What it does not guarantee: that the code now running is trustworthy,
// vetted, or in any way different from whatever was invoked. The installed
// copy is exactly the invoked binary's own bytes, nothing more — a binary
// built from a branch still runs that branch's code once relocated;
// relocation moves where the program's file lives, not what it does.
// Running only a trusted, pinned build regardless of which binary invoked
// `worktree remove` is a separate, later guarantee tied to how releases are
// distributed (#85), not this one.
//
// It runs before Context builds anything and before any lock is taken.
// That ordering is load-bearing two ways: Go opens file descriptors
// close-on-exec by default, so a lock's fd would not survive the exec
// below, and nothing this function's caller has done yet needs undoing if
// it falls back instead of relocating. It is also why this always
// relocates rather than first checking whether the running binary already
// looks like it is outside a checkout: that would need the checkout's own
// path, which nothing knows yet — Context, which loads the config that
// finds it, has not run — so the only binaries that ever skip relocation
// are the ones already inside this store (the canonical-path check below),
// found out the same way regardless of where they started.
//
// A problem at any step — reading the running binary, installing it,
// exec-ing the installed copy — falls back to continuing with the binary
// already running, after telling the log why, rather than failing the
// command outright: relocation hardens `worktree remove` against a
// checkout deleting the program removing it, it is not the ownership check
// every resource still goes through regardless of which binary performs
// the removal, and it does not vet the binary it relocates.
func relocateAndReexec(env *Env) {
	if env.Executable == nil || env.Exec == nil {
		return
	}
	current, err := env.Executable()
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not find its own running binary before removing: %s; continuing with this binary.", err))
		return
	}
	content, err := os.ReadFile(current)
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not read its own running binary before removing: %s; continuing with this binary.", err))
		return
	}
	// Through checkedHome, not layout.ResolveControlHome directly (#80): the
	// berth root gets the same files.AssertSafeHome check every other verb
	// applies to it, and userHome — refused outright if empty — goes on to
	// SecureBinDirectory's own files.AssertAdoptableControlHome check, same
	// as every other control-home directory. Resolved again by Context
	// moments later if relocation falls through to it; both resolutions are
	// cheap and deterministic, so doing it twice costs nothing and skipping
	// the checks here is not an option.
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not resolve the control home before removing: %s; continuing with this binary.", err))
		return
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not resolve the control home before removing: %s; continuing with this binary.", err))
		return
	}
	control := layout.ControlLayout(controlHome)
	if err := files.SecureBinDirectory(control, userHome); err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not prepare a standalone copy of itself before removing: %s; continuing with this binary.", err))
		return
	}
	now := time.Now()
	installed, err := binstore.Install(control.BinDir, content, 0o700, now)
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not install a standalone copy of itself before removing: %s; continuing with this binary.", err))
		return
	}
	// Best-effort, and run synchronously here rather than deferred: a
	// successful Exec below replaces this process and never returns, so
	// anything deferred after this point would never run on the normal
	// path, and the store would grow forever.
	_ = binstore.Prune(control.BinDir, installed, standaloneMaxAge, standaloneMaxEntries, standaloneGrace, now)

	if canonical(current) == canonical(installed) {
		// Already running the installed copy — a previous relocation's own
		// exec, or someone invoking the store's copy directly. Nothing left
		// to relocate; the caller proceeds in this process.
		return
	}
	if len(env.Args) == 0 {
		return
	}
	if err := env.Exec(installed, env.Args, env.Environ); err != nil {
		// Exec only returns on failure; success replaces this process and
		// never reaches here.
		env.Logger.Warn(fmt.Sprintf("Could not run its own standalone copy before removing: %s; continuing with this binary.", err))
	}
}

// canonical is path with every symlink resolved, falling back to path
// itself (cleaned) when it cannot be resolved — a plain, unexported
// counterpart to internal/files's own private canonical, needed here so two
// paths that reach the same file through different symlinks (the control
// home living under a symlinked $HOME, for one) still compare equal.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
