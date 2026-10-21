package commands

import (
	"errors"
	"fmt"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/text"
)

// teardownPinChecks is doctor's look at the teardown pin that `worktree remove`
// runs (docs/design/teardown.md), without executing anything. It uses the
// gate's own pieces (OpenBinary, newerHint, and the same three store checks),
// so what doctor says is what a removal would do.
//
// registry.teardown-pin is one line about the pin and its binary: ok when both
// are intact; a warning when there is no pin yet, the pin has no binary for this
// platform, or the running build is newer than the pin (removal refuses, or
// runs an older build, but nothing is broken); a failure when the pin cannot be
// read, is foreign or newer, or its stored binary is missing or different.
// registry.teardown-store adds a line per problem with where the pin lives, the
// location and ownership ones failures (a pin there is not to be trusted) and
// a noexec mount a warning (removal would refuse, and a remount fixes it).
func teardownPinChecks(opts doctorOptions) []Check {
	name, version := opts.CommandName, opts.Version
	control := layout.ControlLayout(opts.ControlHome)
	const subject = "teardown pin"
	pin, found, err := teardown.ReadPin(control)
	switch {
	case errors.Is(err, stateformat.ErrNewer), errors.Is(err, teardown.ErrForeign):
		return []Check{{Status: StatusFail, ID: "registry.teardown-pin", Subject: subject, Detail: text.OneLine(err.Error(), 300)}}
	case err != nil:
		return []Check{{Status: StatusFail, ID: "registry.teardown-pin", Subject: subject, Detail: fmt.Sprintf("could not be read: %s", text.OneLine(err.Error(), 300))}}
	case !found:
		return []Check{{Status: StatusWarn, ID: "registry.teardown-pin", Subject: subject,
			Detail: fmt.Sprintf("none pinned; `%s worktree remove` will refuse until you pin one with %s", name, pinCommandFor(name, version))}}
	}

	platform := teardown.PlatformKey(opts.OS, opts.Arch)
	check, binary := teardown.OpenBinary(control, pin, platform)
	defer binary.Close()
	again := repinCommand(name, version, pin, platform)
	which := fmt.Sprintf("release %s (%s)", text.Printable(pin.Release), platform)
	var checks []Check
	switch check.State {
	case teardown.BinaryNone:
		checks = append(checks, Check{Status: StatusWarn, ID: "registry.teardown-pin", Subject: subject,
			Detail: fmt.Sprintf("%s has no binary for %s, so `%s worktree remove` will refuse here; pin one for this machine with %s", text.Printable(pin.Release), platform, name, again)})
	case teardown.BinaryOK:
		if hint := newerHint(name, pin, version); hint != "" {
			checks = append(checks, Check{Status: StatusWarn, ID: "registry.teardown-pin", Subject: subject, Detail: which + ": " + hint})
		} else {
			checks = append(checks, Check{Status: StatusOK, ID: "registry.teardown-pin", Subject: subject, Detail: which + ", binary intact"})
		}
	default:
		checks = append(checks, Check{Status: StatusFail, ID: "registry.teardown-pin", Subject: subject,
			Detail: fmt.Sprintf("%s: the stored binary %s %s, so `%s worktree remove` will refuse; pin it again with %s", which, text.Printable(check.Path), check.Problem, name, again)})
	}

	if problem, _ := locationProblem(control, teardown.HomeFromUserDatabase(opts.PasswdHome, opts.UserHome)); problem != "" {
		checks = append(checks, Check{Status: StatusFail, ID: "registry.teardown-store", Instance: "location", Subject: "teardown store", Detail: problem})
	}
	if problem, _ := ownershipProblem(name, version, control, binary); problem != "" {
		checks = append(checks, Check{Status: StatusFail, ID: "registry.teardown-store", Instance: "ownership", Subject: "teardown store", Detail: problem})
	}
	if controlHomeNoExec(opts.NoExec, control) {
		checks = append(checks, Check{Status: StatusWarn, ID: "registry.teardown-store", Instance: "noexec", Subject: "teardown store", Detail: noExecAdvice(name, control)})
	}
	return checks
}
