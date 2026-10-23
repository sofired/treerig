package commands

// The removal gate (#146, docs/design/teardown.md, "How `worktree remove` uses
// it"). It is the first act of `worktree remove`, before Context is built and
// before any lock is taken: a lock's descriptor is close-on-exec and would not
// survive the exec, and nothing has been mutated yet. It runs the pinned,
// verified teardown binary in place of whichever build was invoked, and refuses
// when it cannot; `--dev-teardown` is the one per-invocation way around it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/semver"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/text"
)

// removeTarget is how a refusal names what it was asked to remove.
func removeTarget(options RemoveOptions) string {
	if options.Identifier == "" {
		return "this worktree"
	}
	return "the worktree " + text.Printable(options.Identifier)
}

// gateRefusal is an expected failure whose text is the whole report. It
// answers errors.Is for what it was caused by (a pin's ErrForeign, the exec's
// errno) without exposing it through Unwrap, because the reporter prints an
// unwrapped cause as a second "caused by" line and the refusal already names
// the reason.
type gateRefusal struct {
	message string
	cause   error
}

// Error is the refusal's message.
func (e *gateRefusal) Error() string { return e.message }

// ExitStatus is 1, like any expected failure.
func (e *gateRefusal) ExitStatus() int { return 1 }

// Cause is what the refusal was caused by, or nil; for tests and debugging.
func (e *gateRefusal) Cause() error { return e.cause }

// Is reports whether target is, or is wrapped by, the cause.
func (e *gateRefusal) Is(target error) bool { return e.cause != nil && errors.Is(e.cause, target) }

// As lets errors.As reach the cause's type, for the same reason as Is.
func (e *gateRefusal) As(target any) bool { return e.cause != nil && errors.As(e.cause, target) }

// refuseRemove is the gate's refusal: "Refusing to remove <target>: <detail>
// Nothing was removed." detail is the reason and the next step, each ending in
// a full stop. cause, when non-nil, is kept for errors.Is.
func refuseRemove(target, detail string, cause error) error {
	return &gateRefusal{message: fmt.Sprintf("Refusing to remove %s: %s Nothing was removed.", target, detail), cause: cause}
}

// newerHint is the sentence `teardown status` prints when the running build is
// newer than the pinned release, or "" when it is not (or the pin is a file
// pin, whose release is a free label).
func newerHint(commandName string, pin teardown.Pin, version string) string {
	if pin.IsFile() || !semver.Newer(version, pin.Release) {
		return ""
	}
	return fmt.Sprintf("The running build (%s) is newer than the pinned release (%s). Pin the new build with %s to update it.",
		text.Printable(version), text.Printable(pin.Release), pinCommandFor(commandName, version))
}

// needsConfigHint is the sentence that goes with the notice of a run through a
// pinned release older than the running build, when there is no treerig.toml
// to find from here (no --config, none above the working directory). This
// build removes a named worktree from anywhere, but an older pin (0.1.x)
// reads a treerig.toml first, knows only a slug or a UUID, and stops with "No
// treerig.toml found", which would read as if the worktree needed one. All treerig knows is
// that the pin is older, so it says "may", and gives the two ways that work
// with every pin. A file pin is the caller's own binary, a pin that is not
// older than the running build is as capable as it is, and a development build
// is older than no release, so none of them gets the hint; "" when there is
// nothing to say.
//
// TODO(#210): the teardown-run contract replaces this guess with a probe, and
// the gate runs an old pin from the target's main worktree with --config.
func needsConfigHint(env *Env, version string, pin teardown.Pin) string {
	if pin.IsFile() || !semver.Newer(version, pin.Release) || env.ConfigPath != "" {
		return ""
	}
	if _, found, err := config.FindConfigFile(env.Cwd); err != nil || found {
		return ""
	}
	return fmt.Sprintf("This pin (%s) may need a %s: if it stops with \"No %s found\", run the removal from inside the worktree's repository, or pass --config <that repository's %s>.",
		text.Printable(pin.Release), brand.ConfigFile, brand.ConfigFile, brand.ConfigFile)
}

// withHint puts hint, when there is one, after detail.
func withHint(detail, hint string) string {
	if hint == "" {
		return detail
	}
	return detail + " " + hint
}

// childEnviron is environ without the variables that change what a program
// loads or how its runtime behaves before its first line runs: the dynamic
// loader's (LD_*, DYLD_*) and the Go runtime's and toolchain's (GODEBUG,
// GOFLAGS). A branch's environment file could set any of them to put its own
// code into the pinned binary. It filters what it is given and reads nothing
// itself, as every library function here must.
func childEnviron(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || name == "GODEBUG" || name == "GOFLAGS" {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// runningKind says what the gate takes the running binary to be.
type runningKind int

const (
	// runningOther is any build that is neither of the two below.
	runningOther runningKind = iota
	// runningPinned lives at <control home>/teardown/<its own sha256>.
	runningPinned
	// runningRelocated lives at <control home>/bin/<its own sha256>.
	runningRelocated
)

// identifyRunning works out what the running binary is: the canonical path of
// env.Executable, and, only when that path is named like a stored binary (a
// 64-hex name directly in the teardown or bin directory), whether its bytes
// are that name. The directories are compared with os.SameFile, not as
// strings, so a control home reached through another spelling (a
// case-insensitive volume's, say) is still the same directory: a pinned child
// that failed to recognise itself would exec the stored binary again forever.
// For the same reason, err is non-nil when env.Executable fails, and when a
// file named like a stored binary cannot be read; the gate refuses on it
// (without --dev-teardown, which does not depend on the answer).
func identifyRunning(env *Env, control layout.Control) (path string, kind runningKind, err error) {
	current, err := env.Executable()
	if err != nil {
		return "", runningOther, err
	}
	path = canonical(current)
	name := filepath.Base(path)
	if !teardown.ValidSHA256(name) {
		return path, runningOther, nil
	}
	here, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return path, runningOther, nil
	}
	for _, candidate := range []struct {
		dir  string
		kind runningKind
	}{{control.TeardownDir, runningPinned}, {control.BinDir, runningRelocated}} {
		there, err := os.Stat(candidate.dir)
		if err != nil || !os.SameFile(here, there) {
			continue
		}
		hash, err := binstore.HashFile(path)
		if err != nil {
			return path, runningOther, fmt.Errorf("could not read %s: %s", text.Printable(path), reasonWithoutPath(err))
		}
		if hash == name {
			return path, candidate.kind, nil
		}
	}
	return path, runningOther, nil
}

// readPinForGate reads the pin, turning each way that can fail into the gate's
// own refusal. found is false, with a nil error, when there is no pin: what to
// say then depends on whether the caller is a pinned child. A newer pin keeps
// state.md's refusal as it stands.
func readPinForGate(env *Env, version string, control layout.Control, target string) (pin teardown.Pin, found bool, err error) {
	command := pinCommandFor(env.CommandName, version)
	pin, found, err = teardown.ReadPin(control)
	switch {
	case err == nil:
		return pin, found, nil
	case errors.Is(err, stateformat.ErrNewer):
		return teardown.Pin{}, false, err
	case errors.Is(err, teardown.ErrForeign):
		return teardown.Pin{}, false, refuseRemove(target, fmt.Sprintf("the teardown pin %s is not one this build wrote. Delete %s/, then pin again with %s.",
			text.Printable(control.PinPath), text.Printable(control.TeardownDir), command), err)
	}
	var cause error = err
	var wrapped *clierr.Error
	if errors.As(err, &wrapped) && wrapped.Cause != nil {
		cause = wrapped.Cause
	}
	return teardown.Pin{}, false, refuseRemove(target, fmt.Sprintf("the teardown pin %s could not be read (%s). Check its permissions, or delete %s/ and pin again with %s.",
		text.Printable(control.PinPath), text.OneLine(reasonWithoutPath(cause), 200), text.Printable(control.TeardownDir), command), err)
}

// locationProblem is "" when the control home is outside every git work tree,
// and otherwise the sentence saying it is not: a branch could have written a
// pin and a binary there and pointed TREERIG_CONTROL_HOME at them. cause is
// the error behind it.
func locationProblem(control layout.Control, boundary teardown.HomeBoundary) (problem string, cause error) {
	err := teardown.CheckLocation(control, boundary)
	if err == nil {
		return "", nil
	}
	var inside *teardown.InsideWorkTreeError
	if errors.As(err, &inside) {
		return fmt.Sprintf("%s/ is inside the git work tree %s/, where a branch could plant its own teardown pin. Keep the control home outside every checkout: move TREERIG_HOME (or TREERIG_CONTROL_HOME, if you set it) to a directory that is not inside a git work tree.",
			text.Printable(inside.Path), text.Printable(inside.Root)), err
	}
	return fmt.Sprintf("could not check where the control home is (%s).", text.OneLine(err.Error(), 200)), err
}

// ownershipProblem is "" when everything in the pin store is private to the
// user running treerig, and otherwise the sentence saying what is not: it is
// owned by another user, or its group or others can write it.
func ownershipProblem(commandName, version string, control layout.Control, binary *teardown.Binary) (problem string, cause error) {
	err := teardown.CheckOwnership(control, binary)
	if err == nil {
		return "", nil
	}
	var untrusted *teardown.UntrustedError
	if errors.As(err, &untrusted) {
		// Directories are written with a trailing slash everywhere in these
		// messages, so the path named as the problem and the one to delete are
		// the same form. Deleting teardown/ cannot fix the control home itself.
		shown := text.Printable(untrusted.Path)
		if untrusted.Path == control.ControlHome || untrusted.Path == control.TeardownDir {
			shown += "/"
		}
		steps := fmt.Sprintf("Do one of these: fix its owner and mode; delete %s/ and pin again with %s; or move the control home to a local filesystem with TREERIG_CONTROL_HOME.",
			text.Printable(control.TeardownDir), pinCommandFor(commandName, version))
		if untrusted.Path == control.ControlHome {
			steps = "Fix its owner and mode, or move the control home to a local filesystem with TREERIG_CONTROL_HOME."
		}
		return fmt.Sprintf("%s %s, so the teardown pin is not private to you. %s", shown, untrusted.Problem, steps), err
	}
	return fmt.Sprintf("could not check who owns the teardown pin (%s).", text.OneLine(err.Error(), 200)), err
}

// teardownGate decides what `worktree remove` runs. It returns nil when the
// removal should go on in this process, an error to refuse, and does not
// return at all when it has exec-ed the pinned binary. With either seam nil
// (cli.Main, and every hand-built test Env) it does nothing: only
// cli.MainProcess wires them, and that is where the gate fails closed (for a
// development build, and for any pin that exists).
//
// The checks, in order, with every path compared canonically:
//
//  1. The running binary is <control home>/teardown/<its own hash>: a pinned
//     child. It refuses --dev-teardown, confirms that the pin still names it,
//     says so, and proceeds.
//  2. It is <control home>/bin/<its own hash> and --dev-teardown was given: the
//     relocated copy of a dev run, which proceeds silently.
//  3. --dev-teardown: warn, relocate through bin/ (relocateAndReexec), proceed.
//  4. Otherwise read the pin (a foreign, newer or unreadable one refuses). With no
//     pin, a release build (semver.IsPublishedRelease) removes with itself,
//     relocated through bin/ as before the gate (#7), and a development build
//     refuses; the location check below is not reached, because there is no pin
//     for a branch to have planted. With a pin, which wins for every build:
//     refuse if the running build cannot identify itself (it might be the pinned
//     child), refuse a control home inside a git work tree, open and verify the
//     stored binary for this platform, refuse a store that is not private to
//     this user, and exec the binary through the descriptor that was verified,
//     with the caller's argv and an environment without loader variables. Every
//     failure of a pin that exists refuses.
func teardownGate(ctx context.Context, env *Env, options RemoveOptions) error {
	if env.Executable == nil || env.Exec == nil {
		return nil
	}
	target := removeTarget(options)
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return refuseRemove(target, strings.TrimSuffix(text.Printable(err.Error()), ".")+".", err)
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return refuseRemove(target, strings.TrimSuffix(text.Printable(err.Error()), ".")+".", err)
	}
	control := layout.ControlLayout(controlHome)
	running, kind, runningErr := identifyRunning(env, layout.ControlLayout(canonical(controlHome)))
	name, version := env.CommandName, options.Version
	platform := teardown.PlatformKey(env.OS, env.Arch)

	// 1. A pinned child.
	if kind == runningPinned {
		if options.DevTeardown {
			return clierr.New("Refusing --dev-teardown: this is a pinned teardown binary. Nothing was removed.")
		}
		pin, found, err := readPinForGate(env, version, control, target)
		if err != nil {
			return err
		}
		// An absent pin has no platforms, so it fails the same comparison.
		if pin.Platforms[platform].BinarySHA256 != filepath.Base(running) {
			again := pinCommandFor(name, version)
			if found {
				again = repinCommand(name, version, pin, platform)
			}
			return refuseRemove(target, fmt.Sprintf("this teardown binary %s is not the one the pin names, or the pin was replaced just now. Run this again; if it refuses again, pin it again with %s.",
				text.Printable(running), again), nil)
		}
		env.Logger.Info(fmt.Sprintf("Running as the pinned teardown binary (version %s).", text.Printable(version)))
		return nil
	}

	if options.DevTeardown {
		// 2. The relocated copy of a --dev-teardown run: the flag travelled
		// with the argv, and the parent has already warned.
		if kind == runningRelocated {
			return nil
		}
		// 3. This build's own code, from a copy outside the checkout. If this
		// build cannot be hashed the warning says "unknown" and the removal
		// goes on: relocation reports whatever it cannot do, and the warning is
		// the point.
		shown := "unknown"
		if runningErr == nil {
			if hash, err := binstore.HashFile(running); err == nil {
				shown = hash
			}
		}
		env.Logger.Warn(fmt.Sprintf("Removing with this build's own code (version %s, sha256 %s), not a pinned teardown binary: code from this build runs during the removal.",
			text.Printable(options.Version), shown))
		return relocateAndReexec(ctx, env)
	}

	// 4. No --dev-teardown, and not a pinned child. A usable pin wins for every
	// build; with none, a release build removes with itself and a development
	// build refuses.
	pin, found, err := readPinForGate(env, version, control, target)
	if err != nil {
		return err
	}
	if !found {
		// No pin. A release build is the code users are meant to run, so it
		// removes with itself, relocated through bin/ as before the gate (#7), with
		// nothing said. A development build is where branch code runs, so it
		// refuses unless --dev-teardown was given (handled above).
		if semver.IsPublishedRelease(version) {
			return relocateAndReexec(ctx, env)
		}
		return refuseRemove(target, noPinRefusal(name, version), nil)
	}
	// A pin exists. A build that cannot tell which file it is refuses here: it
	// might be the pinned child itself, and would exec the stored binary again
	// forever. (With no pin it cannot be one, which is why this comes after.)
	if runningErr != nil {
		return refuseRemove(target, fmt.Sprintf("could not tell which treerig build this is (%s).", text.OneLine(reasonWithoutPath(runningErr), 200)), runningErr)
	}
	// A pin exists, so it wins for every build: from here on the control home
	// must be one a branch could not have written, and the pin must be usable.
	if problem, cause := locationProblem(control, teardown.HomeFromUserDatabase(env.PasswdHome, userHome)); problem != "" {
		return refuseRemove(target, problem, cause)
	}
	check, binary := teardown.OpenBinary(control, pin, platform)
	hint := newerHint(name, pin, version)
	again := repinCommand(name, version, pin, platform)
	switch check.State {
	case teardown.BinaryNone:
		return refuseRemove(target, withHint(fmt.Sprintf("the teardown pin has no binary for %s. Pin one for this machine with %s.", platform, again), hint), nil)
	case teardown.BinaryOK:
	default:
		return refuseRemove(target, withHint(fmt.Sprintf("the pinned teardown binary %s %s. Pin it again with %s.",
			text.Printable(check.Path), check.Problem, again), hint), nil)
	}
	defer binary.Close()
	if problem, cause := ownershipProblem(name, version, control, binary); problem != "" {
		return refuseRemove(target, problem, cause)
	}

	execPath, err := binary.ExecPath()
	if err != nil {
		return refuseRemove(target, fmt.Sprintf("could not run the pinned teardown binary %s (%s). Pin it again with %s.",
			text.Printable(check.Path), text.OneLine(err.Error(), 200), again), err)
	}
	if len(env.Args) == 0 {
		return refuseRemove(target, fmt.Sprintf("could not run the pinned teardown binary %s (this process has no arguments to pass it). Pin it again with %s.",
			text.Printable(check.Path), again), nil)
	}
	// A signal caught by this process would be forgotten by the exec, and the
	// new image would remove anyway: stop here so the process dies by it. A
	// signal that lands after this check and before the exec is not seen, and
	// the pinned binary installs its own handlers as it starts; the window is
	// the few instructions between here and the system call.
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	notice := fmt.Sprintf("Running the pinned teardown binary %s (release %s) to remove %s.",
		text.Printable(check.Path), text.Printable(pin.Release), target)
	env.Logger.Info(withHint(withHint(notice, hint), needsConfigHint(env, version, pin)))
	// The caller's argv[0] is kept, so the program still calls itself trig or
	// treerig and not the hash it is stored under.
	err = env.Exec(execPath, pinnedChildArgs(env.Args), childEnviron(env.Environ))
	// Exec returns only when it failed; a nil return from a fake is refused
	// too, since nothing may go on in this process unpinned.
	if err == nil {
		err = errors.New("the exec call returned without starting it")
	}
	reason := text.OneLine(reasonWithoutPath(err), 200)
	switch {
	case errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC):
		return refuseRemove(target, fmt.Sprintf("could not run the pinned teardown binary %s (%s). If the filesystem holding the control home (%s) cannot run programs, remount it with exec, or move the control home with TREERIG_CONTROL_HOME.",
			text.Printable(check.Path), reason, text.Printable(controlHome)), err)
	case errors.Is(err, syscall.ENOENT) && env.OS == "linux":
		// The path handed to exec was /proc/self/fd/<n>: without /proc there is
		// no way to run the file that was verified, and running its path instead
		// would give up the guarantee.
		return refuseRemove(target, fmt.Sprintf("could not run the pinned teardown binary %s (%s). treerig runs the file it verified through /proc/self/fd, and /proc is not available to this process; mount /proc, or run treerig where /proc is available.",
			text.Printable(check.Path), reason), err)
	}
	return refuseRemove(target, fmt.Sprintf("could not run the pinned teardown binary %s (%s). Pin it again with %s.",
		text.Printable(check.Path), reason, again), err)
}

// removeValueFlags are the flags of `worktree remove` that take their value
// as the next argument (--flag value, not --flag=value). The list is kept
// here by hand: this package cannot see the command line's flag set.
var removeValueFlags = map[string]bool{"--domain": true, "--label-namespace": true, "--config": true}

// pinnedChildArgs is argv as the pinned teardown binary is run with it: the
// caller's own arguments without -v/--verbose and --progress, which an older
// pinned build does not know and would refuse after the gate has already
// announced the removal. The value of a flag in removeValueFlags is never stripped. Arguments
// after a bare "--" and argv[0] are kept as they are. A -v inside a cluster of other short flags is left alone: no
// flag of `worktree remove` has a short form but -h and -v.
//
// TODO(#210): the stable teardown-run contract replaces this filter.
func pinnedChildArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := []string{args[0]}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			return append(out, rest[i:]...)
		case removeValueFlags[a] && i+1 < len(rest):
			// The next token is this flag's value, whatever it looks like.
			out = append(out, a, rest[i+1])
			i++
		case a == "--verbose", strings.HasPrefix(a, "--verbose="), strings.HasPrefix(a, "--progress="):
		case a == "--progress" && i+1 < len(rest):
			i++ // and its value
		case len(a) > 1 && a[0] == '-' && strings.Trim(a[1:], "v") == "":
			// -v, or -vv
		default:
			out = append(out, a)
		}
	}
	return out
}
