// Package procid answers what can be proven about a process from outside it,
// and nothing more. A lock holder writes its identity into its record when it
// takes a lock; a later reader checks that identity against the machine, this
// boot of it, and the process table — never against a wall clock, which can
// step. Linux reads /proc directly (no `ps`, which slim images lack and
// BusyBox answers differently); macOS asks the kernel through sysctl.
//
// Holding answers who currently holds a flock — needed only because this
// build's lock design uses one.
package procid

// Identity is who a process is, in the terms a later reader can check it by.
type Identity struct {
	// MachineID is /etc/machine-id on Linux, the platform UUID on macOS.
	MachineID string `json:"machineId,omitempty"`
	// BootID is this boot of the machine. A different one means every
	// process of the earlier boot is gone.
	BootID string `json:"bootId,omitempty"`
	// PIDNS is, on Linux, the PID namespace a PID means something in.
	PIDNS string `json:"pidNs,omitempty"`
	// StartTicks is, on Linux, the process's start in clock ticks since
	// boot — no wall clock involved.
	StartTicks *uint64 `json:"startTicks,omitempty"`
	// StartUs is, on macOS, the process's start in microseconds since the epoch.
	StartUs *int64 `json:"startUs,omitempty"`
}

// State is a process's state, as far as this platform can observe it from outside.
type State int

const (
	// Absent means the PID does not exist.
	Absent State = iota
	// Zombie means the PID exited and is waiting to be reaped.
	Zombie
	// Stopped means the process is suspended, typically by SIGSTOP.
	Stopped
	// Running covers every other live state.
	Running
)

// String names the state the way a notice or log line should show it.
func (s State) String() string {
	switch s {
	case Absent:
		return "absent"
	case Zombie:
		return "zombie"
	case Stopped:
		return "stopped"
	case Running:
		return "running"
	default:
		return "unknown"
	}
}

// Status is what Inspect can tell about one PID: its state, and its start so
// a reused PID can be told apart from the one a record named.
type Status struct {
	State      State
	StartTicks *uint64
	StartUs    *int64
}

// Proc is one scanned process: its PID, and what could be read of its
// argument list and environment.
type Proc struct {
	PID  int
	Args []string
	// Env holds "NAME=value" entries; EnvRead is false when the environment
	// could not be read at all, as distinct from a process with none.
	Env     []string
	EnvRead bool
}

// Scan is every other process in this PID namespace, with what could be read of it.
type Scan struct {
	Procs []Proc
	// Unreadable counts this user's own processes whose command line or
	// environment could not be read.
	Unreadable int
	// OthersHidden is true when other users' processes are hidden from this
	// one (Linux /proc mounted with hidepid, say), so a command the holder
	// ran as someone else — through sudo, say — could not be looked for.
	OthersHidden bool
}

// Markers identify a worktree's own processes: its Compose project name on
// the command line, or its worktree ID in the environment.
type Markers struct {
	Project    string
	WorktreeID string
}

// Table is how this platform is asked about processes.
type Table interface {
	// Self is this process, as a lock record describes it.
	Self() (Identity, error)
	// Inspect reports whether pid exists, and in what state, with its start
	// to tell a reused PID apart.
	Inspect(pid int) (Status, error)
	// Scan lists every other process in this PID namespace, with what could be read of it.
	Scan() (Scan, error)
	// Holding lists the processes holding an open descriptor on the file
	// identified by (dev, ino) — Linux only; darwin returns errors.ErrUnsupported.
	Holding(dev, ino uint64) ([]Proc, error)
}

// Marked returns the processes started for a worktree: its Compose project
// name as a whole argv token, or TREERIG_WORKTREE_ID=<wt> / COMPOSE_PROJECT_NAME=<p>
// in its environment.
func Marked(procs []Proc, m Markers) []Proc {
	wantWorktree := "TREERIG_WORKTREE_ID=" + m.WorktreeID
	wantProject := "COMPOSE_PROJECT_NAME=" + m.Project
	var found []Proc
	for _, p := range procs {
		if containsToken(p.Args, m.Project) || containsEnv(p.Env, wantWorktree, wantProject) {
			found = append(found, p)
		}
	}
	return found
}

// containsToken reports whether want appears as a whole argv entry in tokens.
func containsToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}

// containsEnv reports whether env contains any of wants as a whole "NAME=value" entry.
func containsEnv(env []string, wants ...string) bool {
	for _, e := range env {
		for _, want := range wants {
			if e == want {
				return true
			}
		}
	}
	return false
}
