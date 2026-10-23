package worktree

import (
	"fmt"
	"strconv"
	"strings"
)

// State is where a worktree stands, as every command, the overview page and
// every --json document say it (docs/design/cli-1.0.md, "State words"). A
// worktree is in exactly one of these, which Classify decides from plain
// inputs and nothing else does.
type State string

// The worktree states, in the order Classify tries them: the first that
// applies wins.
const (
	// StateRemoving is a worktree whose removal was started and not finished.
	StateRemoving State = "removing"
	// StateDirectoryMissing is a worktree Git, asked successfully, no longer
	// has the directory of (see Directory).
	StateDirectoryMissing State = "directory missing"
	// StateUnknown is a worktree whose state cannot be told because Docker or
	// Git did not answer, or a service is in a state treerig does not know.
	StateUnknown State = "unknown"
	// StateStarting is a worktree with a running container whose health check
	// has not passed yet.
	StateStarting State = "starting"
	// StateFailed is a worktree with no service running and at least one
	// that exited with an error, is restarting or is unhealthy.
	StateFailed State = "failed"
	// StatePartlyRunning is a worktree with some service running and some
	// other that is not.
	StatePartlyRunning State = "partly running"
	// StateRunning is a worktree with a service running and every other one
	// either running or finished cleanly.
	StateRunning State = "running"
	// StateStopped is a worktree an `up` has finished for, with nothing
	// running now.
	StateStopped State = "stopped"
	// StateNotStarted is a registered worktree no `up` has ever finished for.
	StateNotStarted State = "not started"
)

// States is every worktree state, in precedence order.
var States = []State{
	StateRemoving, StateDirectoryMissing, StateUnknown, StateStarting, StateFailed,
	StatePartlyRunning, StateRunning, StateStopped, StateNotStarted,
}

// String is the state as text output prints it.
func (s State) String() string { return string(s) }

// JSON is the state as every --json document spells it: the text word in
// snake case. An explicit table, not a string replacement, so the mapping is
// pinned word by word.
func (s State) JSON() string {
	switch s {
	case StateRemoving:
		return "removing"
	case StateDirectoryMissing:
		return "directory_missing"
	case StateUnknown:
		return "unknown"
	case StateStarting:
		return "starting"
	case StateFailed:
		return "failed"
	case StatePartlyRunning:
		return "partly_running"
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateNotStarted:
		return "not_started"
	}
	return strings.ReplaceAll(string(s), " ", "_")
}

// MarshalText makes encoding/json write a State in its JSON spelling, so a
// document that holds one can never print the text word by mistake.
func (s State) MarshalText() ([]byte, error) { return []byte(s.JSON()), nil }

// UnmarshalText reads a State from its JSON spelling.
func (s *State) UnmarshalText(text []byte) error {
	for _, state := range States {
		if state.JSON() == string(text) {
			*s = state
			return nil
		}
	}
	return fmt.Errorf("%q is not a worktree state", text)
}

// ServiceState is where one service (one container of it) stands.
type ServiceState string

// The service states.
const (
	// Running is a running container that is healthy, or has no health check.
	Running ServiceState = "running"
	// Starting is a running container whose health check has not passed yet.
	Starting ServiceState = "starting"
	// Unhealthy is a running container failing its health check.
	Unhealthy ServiceState = "unhealthy"
	// Restarting is a container Docker keeps restarting.
	Restarting ServiceState = "restarting"
	// Exited is a container that ran and ended; ServiceStatus.ExitCode says how.
	Exited ServiceState = "exited"
	// Stopped is a container that exists and does not run, and did not crash:
	// created and never started, paused, dead, being removed, or ended by
	// SIGKILL or SIGTERM (as `compose stop` and a reboot do).
	Stopped ServiceState = "stopped"
	// NotCreated is a declared service with no container at all, as after `down`.
	NotCreated ServiceState = "not created"
	// Unknown is a container in a state treerig has no word for;
	// ServiceStatus.Detail carries Docker's own.
	Unknown ServiceState = "unknown"
)

// ServiceStates is every service state.
var ServiceStates = []ServiceState{Running, Starting, Unhealthy, Restarting, Exited, Stopped, NotCreated, Unknown}

// String is the state as text output prints it.
func (s ServiceState) String() string { return string(s) }

// JSON is the state as --json spells it: the text word in snake case.
func (s ServiceState) JSON() string {
	switch s {
	case NotCreated:
		return "not_created"
	case Running, Starting, Unhealthy, Restarting, Exited, Stopped, Unknown:
		return string(s)
	}
	return strings.ReplaceAll(string(s), " ", "_")
}

// MarshalText makes encoding/json write a ServiceState in its JSON spelling.
func (s ServiceState) MarshalText() ([]byte, error) { return []byte(s.JSON()), nil }

// UnmarshalText reads a ServiceState from its JSON spelling.
func (s *ServiceState) UnmarshalText(text []byte) error {
	for _, state := range ServiceStates {
		if state.JSON() == string(text) {
			*s = state
			return nil
		}
	}
	return fmt.Errorf("%q is not a service state", text)
}

// ServiceStatus is one declared service's state, or one of its containers'.
type ServiceStatus struct {
	Service string
	State   ServiceState
	// ExitCode is how an Exited service ended; zero for every other state.
	ExitCode int
	// Detail is Docker's own word for an Unknown state; empty otherwise.
	Detail string
}

// Text is the state as a line of `status` prints it: the word, and the exit
// code of one that exited.
func (s ServiceStatus) Text() string {
	if s.State == Exited {
		return "exited (code " + strconv.Itoa(s.ExitCode) + ")"
	}
	return string(s.State)
}

// Exit codes a signal leaves behind: SIGKILL (137, which is also what the
// kernel's out-of-memory killer leaves) and SIGTERM (143). `compose stop`, a
// reboot and a daemon restart end containers this way, so they mean the
// container was stopped, not that it crashed.
const (
	// CauseNotRead is the Detail of a service that exited 137 when Docker was
	// not asked, or would not say, whether the out-of-memory killer did it.
	CauseNotRead = "exited 137, cause not read"

	exitKilled     = 137
	exitTerminated = 143
)

// ContainerStatus reads what Docker says about one container into a
// ServiceStatus (Service left empty): its state word (`running`, `exited`,
// `created`, …), its health (`healthy`, `unhealthy`, `starting`, or "" for a
// container with no health check) and, when it is known, the exit code, and
// whether Docker reports it was killed for running out of memory (oomRead says
// the caller asked Docker, so oomKilled means something).
//
// A `created` container is Stopped: it exists and nothing runs, and an `up`
// that failed to start it leaves it there for good. An exit by SIGTERM (143) is
// Stopped. An exit by SIGKILL (137) is a stop (`compose stop`, a reboot) or the
// kernel's out-of-memory killer, and only Docker says which: Stopped when
// oomRead and not oomKilled, Exited when oomKilled, and Unknown when the caller
// could not read the flag. An exited container whose code is not known, and a
// state word not named here, are Unknown, so nothing guesses that a service is
// finished or stopped.
func ContainerStatus(state, health string, exitCode int, exitKnown, oomKilled, oomRead bool) ServiceStatus {
	switch word := strings.ToLower(state); word {
	case "running":
		switch strings.ToLower(health) {
		case "starting":
			return ServiceStatus{State: Starting}
		case "unhealthy":
			return ServiceStatus{State: Unhealthy}
		}
		return ServiceStatus{State: Running}
	case "restarting":
		return ServiceStatus{State: Restarting}
	case "created":
		return ServiceStatus{State: Stopped}
	case "exited":
		if !exitKnown {
			return ServiceStatus{State: Unknown, Detail: state}
		}
		switch {
		case exitCode == exitTerminated:
			return ServiceStatus{State: Stopped}
		case exitCode == exitKilled && !oomRead:
			return ServiceStatus{State: Unknown, Detail: CauseNotRead}
		case exitCode == exitKilled && !oomKilled:
			return ServiceStatus{State: Stopped}
		}
		return ServiceStatus{State: Exited, ExitCode: exitCode}
	case "paused", "dead", "removing":
		return ServiceStatus{State: Stopped}
	}
	return ServiceStatus{State: Unknown, Detail: state}
}

// Directory is what is known of a worktree's directory on disk.
type Directory int

const (
	// DirectoryNotChecked is the zero value: Git was not asked about this
	// worktree (it belongs to another repository), so the directory says
	// nothing either way.
	DirectoryNotChecked Directory = iota
	// DirectoryPresent is a directory that is there.
	DirectoryPresent
	// DirectoryGone is positive evidence that the directory is gone: Git
	// answered, listed the entry as prunable and not locked, the directory is
	// absent, and its parent exists, is not empty and is not an unmounted
	// mount point (internal/worktreestate decides this).
	DirectoryGone
	// DirectoryUndetermined is a directory that may be gone or may be on a
	// volume that is not mounted; Inputs.DirectoryHint names the cause.
	DirectoryUndetermined
)

// Inputs is everything Classify decides a worktree's state from. They are
// plain values: gathering them is internal/worktreestate's job, so this
// package stays a leaf.
type Inputs struct {
	// Removing is a removal marker: `remove` started and did not finish.
	Removing bool
	// Directory is what is known of the directory, and DirectoryHint why it
	// is DirectoryUndetermined.
	Directory     Directory
	DirectoryHint string
	// RecordError is why a record treerig keeps about the worktree (its
	// removal marker, its index card) could not be read, or "" when all could.
	RecordError string
	// ServicesHint is why the services could not be read (Docker or Compose
	// did not answer), as a sentence for a person, or "" when they could. The
	// caller words it: only it knows which of them failed. Services are then
	// not to be believed.
	ServicesHint string
	// Started is whether an `up` has ever finished (the registration says
	// ready rather than pending). It is history: `down` removes containers,
	// so stopped cannot depend on them existing.
	Started bool
	// Services are the worktree's containers, one entry each: a scaled
	// service appears once per replica, and a declared service with none as
	// NotCreated.
	Services []ServiceStatus
}

// Classification is a worktree's state, with the cause when it is Unknown.
type Classification struct {
	State State
	// Hint names the cause of StateUnknown; empty otherwise.
	Hint string
}

// Classify maps plain inputs to the one state a worktree is in. The first
// rule that applies wins: removing, directory missing, unknown, starting,
// failed, partly running, running, stopped, not started.
//
// Containers decide over history: a first `up` that fails with one service
// running is partly running, and one whose only service crashed is failed. A
// service that exited with code 0 has done its job and counts as neither
// running nor failed, so `web` running beside a finished `migrate` is running,
// and a worktree whose services all finished cleanly is stopped. A running
// worktree is one with a service running and every other service either
// running or finished; any other service (stopped, not created, unhealthy,
// restarting, exited with an error) makes it partly running.
func Classify(in Inputs) Classification {
	if in.Removing {
		return Classification{State: StateRemoving}
	}
	if in.Directory == DirectoryGone {
		return Classification{State: StateDirectoryMissing}
	}
	if in.Directory == DirectoryUndetermined {
		return Classification{State: StateUnknown, Hint: in.DirectoryHint}
	}
	if in.RecordError != "" {
		return Classification{State: StateUnknown, Hint: in.RecordError}
	}
	if in.ServicesHint != "" {
		return Classification{State: StateUnknown, Hint: in.ServicesHint}
	}
	var running, starting, broken, other bool
	for _, service := range in.Services {
		switch service.State {
		case Running:
			running = true
		case Starting:
			starting = true
		case Unhealthy, Restarting:
			broken = true
		case Exited:
			if service.ExitCode != 0 {
				broken = true
			}
		case Stopped, NotCreated:
			other = true
		default:
			// Unknown, a state added later, or the zero value: nothing may
			// guess what it means.
			return Classification{State: StateUnknown, Hint: unknownServiceHint(service)}
		}
	}
	switch {
	case starting:
		return Classification{State: StateStarting}
	case !running && broken:
		return Classification{State: StateFailed}
	case running && (broken || other):
		return Classification{State: StatePartlyRunning}
	case running:
		return Classification{State: StateRunning}
	case in.Started:
		return Classification{State: StateStopped}
	}
	return Classification{State: StateNotStarted}
}

// unknownServiceHint says why a service's state cannot be told.
func unknownServiceHint(service ServiceStatus) string {
	name := "A service"
	if service.Service != "" {
		name = service.Service
	}
	switch {
	case service.State == Unknown && service.Detail == CauseNotRead:
		return "Whether " + name + " was stopped or killed for running out of memory (it exited 137) couldn't be read."
	case service.State == Unknown && service.Detail == "exited":
		return "No exit code was reported for " + name + ", which exited."
	case service.State == Unknown && service.Detail != "":
		return name + " is in a state treerig has no word for (Docker says " + service.Detail + ")."
	case service.State == Unknown || service.State == "":
		return name + " is in a state treerig has no word for."
	}
	return name + " is in a state treerig has no word for (" + string(service.State) + ")."
}

// Summarize collapses containers into one entry per service, in the order
// the services first appear: a scaled service is as bad as its worst replica
// (unknown, restarting, unhealthy, exited with an error, starting, stopped,
// running, finished), so the line a user reads never hides a failed one.
func Summarize(containers []ServiceStatus) []ServiceStatus {
	var order []string
	worst := map[string]ServiceStatus{}
	for _, container := range containers {
		current, seen := worst[container.Service]
		if !seen {
			order = append(order, container.Service)
		}
		if !seen || severity(container) > severity(current) {
			worst[container.Service] = container
		}
	}
	summary := make([]ServiceStatus, len(order))
	for i, name := range order {
		summary[i] = worst[name]
	}
	return summary
}

// severity orders a service's replicas for Summarize, worst highest. A
// replica that is broken outranks one that is starting, so the line a person
// reads names the failure, though Classify says starting first.
func severity(s ServiceStatus) int {
	switch {
	case s.State == Restarting:
		return 6
	case s.State == Unhealthy:
		return 5
	case s.State == Exited && s.ExitCode != 0:
		return 4
	case s.State == Starting:
		return 3
	case s.State == Stopped, s.State == NotCreated:
		return 2
	case s.State == Running:
		return 1
	case s.State == Exited:
		return 0
	}
	// Unknown, or a state this build has no word for.
	return 7
}
