//go:build darwin

package procid

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// BSD process-state codes from bsd/sys/proc.h; not exported by x/sys/unix.
const (
	sstop = 4
	szomb = 5
)

// DarwinTable asks the kernel through sysctl; macOS has no /proc. The macOS
// CI job exercises it through the lock layer's real-process tests: identity,
// exact start times, stopped holders and the marker scan.
type DarwinTable struct{}

// Platform is this platform's process table.
func Platform() Table { return DarwinTable{} }

func startUs(kinfo *unix.KinfoProc) int64 {
	return kinfo.Proc.P_starttime.Sec*1_000_000 + int64(kinfo.Proc.P_starttime.Usec)
}

func (DarwinTable) Self() (Identity, error) {
	var identity Identity
	if kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid()); err == nil {
		us := startUs(kinfo)
		identity.StartUs = &us
	}
	if out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		if uuid, ok := parseIoregUUID(string(out)); ok {
			identity.MachineID = uuid
		}
	}
	if boot, err := unix.Sysctl("kern.bootsessionuuid"); err == nil {
		if boot = strings.TrimSpace(boot); boot != "" {
			identity.BootID = boot
		}
	}
	return identity, nil
}

// Inspect asks kern.proc.pid directly: SysctlKinfoProc reports EIO when the
// kernel answered with no data, which is what an absent PID looks like.
func (DarwinTable) Inspect(pid int) (Status, error) {
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) {
			return Status{State: Absent}, nil
		}
		return Status{}, err
	}
	state := Running
	switch kinfo.Proc.P_stat {
	case sstop:
		state = Stopped
	case szomb:
		state = Zombie
	}
	us := startUs(kinfo)
	return Status{State: state, StartUs: &us}, nil
}

// Scan lists every other process from kern.proc.all, reading each one's exact
// argv and environment from kern.procargs2. procargs2 refuses another user's
// process without root; those fall back to a single `ps` call, as the
// TypeScript build's DarwinProcessTable always did.
func (DarwinTable) Scan() (Scan, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return Scan{}, err
	}
	self := os.Getpid()
	ourUID := os.Getuid()
	seen := make(map[int]bool, len(all))
	var procs []Proc
	unreadable := 0
	var fallback []int
	for _, kinfo := range all {
		pid := int(kinfo.Proc.P_pid)
		if pid == self || pid == 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		if argv, envp, ok := procArgs(pid); ok {
			procs = append(procs, Proc{PID: pid, Args: argv, Env: envp, EnvRead: true})
			continue
		}
		if _, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err != nil {
			continue // gone mid-scan
		}
		if int(kinfo.Eproc.Ucred.Uid) == ourUID {
			unreadable++
			continue
		}
		fallback = append(fallback, pid)
	}
	othersHidden := false
	if len(fallback) > 0 {
		found := psFallback(fallback)
		for _, pid := range fallback {
			if proc, ok := found[pid]; ok {
				procs = append(procs, proc)
				continue
			}
			// Missing from ps: hidden only if it is still there. A process
			// that exited between kern.proc.all and ps is simply gone.
			if _, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil {
				othersHidden = true
			}
		}
	}
	return Scan{Procs: procs, Unreadable: unreadable, OthersHidden: othersHidden}, nil
}

// Holding cannot be answered on macOS: there is no /proc to enumerate open
// descriptors from, and lsof is not guaranteed present or unprivileged.
func (DarwinTable) Holding(dev, ino uint64) ([]Proc, error) {
	return nil, errors.ErrUnsupported
}

func procArgs(pid int) (argv, envp []string, ok bool) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, false
	}
	return parseProcArgs2(raw)
}

// quietEnv is a clean environment: no PS_FORMAT, COLUMNS or locale to reshape ps output.
var quietEnv = []string{"PATH=/bin:/usr/bin:/usr/sbin:/sbin", "LC_ALL=C"}

// psFallback runs `ps -A -ww -E -o pid=,command=` once and returns whichever
// of the wanted PIDs it could see.
func psFallback(want []int) map[int]Proc {
	cmd := exec.Command("/bin/ps", "-A", "-ww", "-E", "-o", "pid=,command=")
	cmd.Env = quietEnv
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	scanned := parsePsScan(string(out), nil)
	wanted := make(map[int]bool, len(want))
	for _, pid := range want {
		wanted[pid] = true
	}
	found := make(map[int]Proc, len(want))
	for _, proc := range scanned {
		if wanted[proc.PID] {
			found[proc.PID] = proc
		}
	}
	return found
}
