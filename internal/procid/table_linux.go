//go:build linux

package procid

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/clierr"
)

// LinuxTable reads /proc itself; root is overridden in tests.
type LinuxTable struct {
	root           string
	machineIDFiles []string
	uid            int
}

// NewLinuxTable is exported for tests, which point root at a fake /proc.
func NewLinuxTable(root string, machineIDFiles []string, uid int) *LinuxTable {
	return &LinuxTable{root: root, machineIDFiles: machineIDFiles, uid: uid}
}

// newDefaultLinuxTable builds a LinuxTable that reads the real /proc and the
// real machine-id files, as the current user.
func newDefaultLinuxTable() *LinuxTable {
	return NewLinuxTable("/proc", []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}, os.Getuid())
}

// Platform is this platform's process table.
func Platform() Table { return newDefaultLinuxTable() }

// firstLine reads file and returns its first line trimmed of surrounding
// whitespace; ok is false if the file is missing, unreadable, or empty.
func firstLine(file string) (string, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	return line, line != ""
}

// Self reports this process's identity from /proc: the machine ID (first
// file found among machineIDFiles), the boot ID, this process's PID
// namespace, and its start in clock ticks since boot. A piece that cannot be
// read is left zero rather than failing the call.
func (t *LinuxTable) Self() (Identity, error) {
	var identity Identity
	for _, file := range t.machineIDFiles {
		if id, ok := firstLine(file); ok {
			identity.MachineID = id
			break
		}
	}
	if id, ok := firstLine(filepath.Join(t.root, "sys", "kernel", "random", "boot_id")); ok {
		identity.BootID = id
	}
	if ns, err := os.Readlink(filepath.Join(t.root, "self", "ns", "pid")); err == nil {
		identity.PIDNS = ns
	}
	if data, err := os.ReadFile(filepath.Join(t.root, "self", "stat")); err == nil {
		if _, ticks, ok := parseProcStat(string(data)); ok {
			identity.StartTicks = &ticks
		}
	}
	return identity, nil
}

// isGone reports whether err means the process (or its /proc entry) has
// already vanished, as opposed to a real failure.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// Inspect reads pid's state and start ticks from its /proc/<pid>/stat. A PID
// that has already gone is reported as Absent rather than an error; a stat
// file that cannot be parsed is a real error, since /proc having no readable
// shape for a process we can stat is unexpected.
func (t *LinuxTable) Inspect(pid int) (Status, error) {
	data, err := os.ReadFile(filepath.Join(t.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		if isGone(err) {
			return Status{State: Absent}, nil
		}
		return Status{}, err
	}
	state, ticks, ok := parseProcStat(string(data))
	if !ok {
		return Status{}, clierr.Newf("could not read the state of process %d from %s", pid, t.root)
	}
	return Status{State: stateOf(state[0]), StartTicks: &ticks}, nil
}

// ownership is whose a process is — or that it has gone. An owner that
// cannot be read is not assumed to be us.
type ownership int

const (
	ownerGone ownership = iota
	ownerOurs
	ownerTheirs
)

// ownerOf reports whether pid belongs to this table's uid, someone else, or
// has already gone. It falls back to ownerTheirs whenever ownership cannot be
// established, so a process is never treated as ours by default.
func (t *LinuxTable) ownerOf(pid int) ownership {
	info, err := os.Stat(filepath.Join(t.root, strconv.Itoa(pid)))
	if err != nil {
		if isGone(err) {
			return ownerGone
		}
		return ownerTheirs
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ownerTheirs
	}
	if int(sys.Uid) == t.uid {
		return ownerOurs
	}
	return ownerTheirs
}

// isAllDigits reports whether s is non-empty and every byte is a decimal
// digit — how a /proc entry is told apart from a PID directory.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// nulSplit splits data on NUL bytes, as /proc/<pid>/cmdline and /environ are
// laid out, dropping the empty trailing entry a terminating NUL leaves.
func nulSplit(data []byte) []string {
	var out []string
	for _, part := range strings.Split(string(data), "\x00") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Scan lists every other process in this PID namespace by walking /proc,
// reading each one's command line and environment where permission allows.
// Processes in a different PID namespace are skipped, since their PIDs mean
// nothing here. What could not be read is folded into Unreadable (ours) or
// OthersHidden (someone else's), rather than silently dropped.
func (t *LinuxTable) Scan() (Scan, error) {
	own, _ := os.Readlink(filepath.Join(t.root, "self", "ns", "pid"))
	entries, err := os.ReadDir(t.root)
	if err != nil {
		return Scan{}, err
	}
	self := os.Getpid()
	var procs []Proc
	unreadable := 0
	// hidepid=2 hides every other user's process from a non-root one, init included.
	othersHidden := t.uid != 0
	if othersHidden {
		if _, err := os.Stat(filepath.Join(t.root, "1")); err == nil {
			othersHidden = false
		}
	}
	for _, entry := range entries {
		if !isAllDigits(entry.Name()) {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if pid == self {
			continue
		}
		// Another namespace's processes are not this worktree's children, and
		// their PIDs mean nothing here: a container's own services, say.
		namespace, nsErr := os.Readlink(filepath.Join(t.root, entry.Name(), "ns", "pid"))
		if own != "" && nsErr == nil && namespace != own {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(t.root, entry.Name(), "cmdline"))
		if err != nil {
			if isGone(err) {
				continue // gone mid-scan
			}
			// hidepid=1: visible, but not readable. Ours would be a gap;
			// someone else's, a blind spot.
			switch t.ownerOf(pid) {
			case ownerOurs:
				unreadable++
			case ownerTheirs:
				othersHidden = true
			}
			continue
		}
		args := nulSplit(cmdline)
		environ, err := os.ReadFile(filepath.Join(t.root, entry.Name(), "environ"))
		if err != nil {
			if isGone(err) {
				continue // gone mid-scan
			}
			// Another user's environment is private on every Linux host, so
			// such a process is judged by its command line alone; counting
			// them would make every scan look blind. Only an unreadable one
			// of ours is a gap worth reporting.
			if t.ownerOf(pid) == ownerOurs {
				unreadable++
			}
			procs = append(procs, Proc{PID: pid, Args: args})
			continue
		}
		procs = append(procs, Proc{PID: pid, Args: args, Env: nulSplit(environ), EnvRead: true})
	}
	return Scan{Procs: procs, Unreadable: unreadable, OthersHidden: othersHidden}, nil
}

// Holding scans /proc/*/fd/* for descriptors on the lock file identified by
// (dev, ino), and lists the processes holding one. A process merely
// contending for the same flock (openly, without holding it) also keeps such
// a descriptor while it polls; this process's own PID is left out for that
// reason, the same way Scan leaves it out.
func (t *LinuxTable) Holding(dev, ino uint64) ([]Proc, error) {
	entries, err := os.ReadDir(t.root)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var procs []Proc
	for _, entry := range entries {
		if !isAllDigits(entry.Name()) {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		fdDir := filepath.Join(t.root, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // gone, or not ours to see
		}
		holds := false
		for _, fd := range fds {
			info, err := os.Stat(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			sys, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			if sys.Dev == dev && sys.Ino == ino {
				holds = true
				break
			}
		}
		if !holds {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(t.root, entry.Name(), "cmdline"))
		if err != nil {
			procs = append(procs, Proc{PID: pid})
			continue
		}
		args := nulSplit(cmdline)
		environ, err := os.ReadFile(filepath.Join(t.root, entry.Name(), "environ"))
		if err != nil {
			procs = append(procs, Proc{PID: pid, Args: args})
			continue
		}
		procs = append(procs, Proc{PID: pid, Args: args, Env: nulSplit(environ), EnvRead: true})
	}
	return procs, nil
}
