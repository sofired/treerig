//go:build linux

package procid

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func fakeProc(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sys", "kernel", "random"))
	mustWriteFile(t, filepath.Join(root, "sys", "kernel", "random", "boot_id"), "boot-1\n")
	mustMkdirAll(t, filepath.Join(root, "self", "ns"))
	mustSymlink(t, "pid:[1]", filepath.Join(root, "self", "ns", "pid"))
	mustWriteFile(t, filepath.Join(root, "self", "stat"), statLine("node", "R", 500))
	return root
}

type procFields struct {
	stat              string
	hasStat           bool
	cmdline           []string
	environ           []string
	environUnreadable bool
	hasEnviron        bool
	ns                string
	hasNS             bool
}

func addProcess(t *testing.T, root string, pid int, fields procFields) {
	t.Helper()
	directory := filepath.Join(root, itoaInt(pid))
	mustMkdirAll(t, filepath.Join(directory, "ns"))
	if fields.hasStat {
		mustWriteFile(t, filepath.Join(directory, "stat"), fields.stat)
	}
	mustWriteFile(t, filepath.Join(directory, "cmdline"), joinNul(fields.cmdline))
	environPath := filepath.Join(directory, "environ")
	if fields.environUnreadable {
		mustMkdirAll(t, environPath) // a directory: unreadable to root too, as a mode-000 file is not.
	} else if fields.hasEnviron {
		mustWriteFile(t, environPath, joinNul(fields.environ))
	}
	if fields.hasNS {
		mustSymlink(t, fields.ns, filepath.Join(directory, "ns", "pid"))
	}
}

func joinNul(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p + "\x00"
	}
	return out
}

func itoaInt(n int) string { return itoa(uint64(n)) }

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxTable(t *testing.T) {
	t.Run("describes itself by machine, boot, namespace and start ticks", func(t *testing.T) {
		root := fakeProc(t)
		machine := filepath.Join(root, "machine-id")
		mustWriteFile(t, machine, "abc123\n")

		identity, err := NewLinuxTable(root, []string{filepath.Join(root, "missing"), machine}, os.Getuid()).Self()
		if err != nil {
			t.Fatal(err)
		}
		if identity.MachineID != "abc123" || identity.BootID != "boot-1" || identity.PIDNS != "pid:[1]" {
			t.Fatalf("got %+v", identity)
		}
		if identity.StartTicks == nil || *identity.StartTicks != 500 {
			t.Fatalf("got StartTicks %v", identity.StartTicks)
		}

		blind, err := NewLinuxTable(root, []string{filepath.Join(root, "missing")}, os.Getuid()).Self()
		if err != nil {
			t.Fatal(err)
		}
		if blind.MachineID != "" {
			t.Fatalf("expected no machine id, got %q", blind.MachineID)
		}
	})

	t.Run("tells an absent, exited, stopped and running process apart, with its start", func(t *testing.T) {
		root := fakeProc(t)
		addProcess(t, root, 7, procFields{stat: statLine("z", "Z", 11), hasStat: true})
		addProcess(t, root, 8, procFields{stat: statLine("t", "T", 12), hasStat: true})
		addProcess(t, root, 9, procFields{stat: statLine("r", "S", 13), hasStat: true})
		addProcess(t, root, 10, procFields{stat: "garbled", hasStat: true})
		table := NewLinuxTable(root, nil, os.Getuid())

		check := func(pid int, wantState State, wantTicks uint64, wantErr bool) {
			t.Helper()
			status, err := table.Inspect(pid)
			if wantErr {
				if err == nil {
					t.Fatalf("Inspect(%d): expected error", pid)
				}
				return
			}
			if err != nil {
				t.Fatalf("Inspect(%d): %v", pid, err)
			}
			if status.State != wantState {
				t.Fatalf("Inspect(%d).State = %v, want %v", pid, status.State, wantState)
			}
			if wantState != Absent {
				if status.StartTicks == nil || *status.StartTicks != wantTicks {
					t.Fatalf("Inspect(%d).StartTicks = %v, want %d", pid, status.StartTicks, wantTicks)
				}
			}
		}
		check(6, Absent, 0, false)
		check(7, Zombie, 11, false)
		check(8, Stopped, 12, false)
		check(9, Running, 13, false)
		check(10, 0, 0, true)
	})

	t.Run("scans this namespace only, counts what it could not read, and leaves itself out", func(t *testing.T) {
		root := fakeProc(t)
		addProcess(t, root, 20, procFields{cmdline: []string{"docker", "compose"}, environ: []string{"A=1"}, hasEnviron: true, ns: "pid:[1]", hasNS: true})
		addProcess(t, root, 21, procFields{cmdline: []string{"service"}, environ: []string{"A=1"}, hasEnviron: true, ns: "pid:[2]", hasNS: true})
		addProcess(t, root, 22, procFields{cmdline: []string{"someone-else"}, environUnreadable: true, ns: "pid:[1]", hasNS: true})
		addProcess(t, root, 23, procFields{cmdline: []string{"no-namespace-link"}, environ: []string{"B=2"}, hasEnviron: true})
		addProcess(t, root, os.Getpid(), procFields{cmdline: []string{"me"}, environ: []string{"C=3"}, hasEnviron: true, ns: "pid:[1]", hasNS: true})
		mustMkdirAll(t, filepath.Join(root, "not-a-pid"))

		scan, err := NewLinuxTable(root, nil, os.Getuid()).Scan()
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(scan.Procs, func(i, j int) bool { return scan.Procs[i].PID < scan.Procs[j].PID })
		want := []Proc{
			{PID: 20, Args: []string{"docker", "compose"}, Env: []string{"A=1"}, EnvRead: true},
			{PID: 22, Args: []string{"someone-else"}},
			{PID: 23, Args: []string{"no-namespace-link"}, Env: []string{"B=2"}, EnvRead: true},
		}
		if len(scan.Procs) != len(want) {
			t.Fatalf("got %+v", scan.Procs)
		}
		for i := range want {
			if scan.Procs[i].PID != want[i].PID || !equalStrings(scan.Procs[i].Args, want[i].Args) ||
				!equalStrings(scan.Procs[i].Env, want[i].Env) || scan.Procs[i].EnvRead != want[i].EnvRead {
				t.Fatalf("got %+v, want %+v", scan.Procs[i], want[i])
			}
		}
		if scan.Unreadable != 1 {
			t.Fatalf("Unreadable = %d, want 1", scan.Unreadable)
		}

		// Another user's private environment is no gap in the proof: a worktree's processes run as its user.
		asSomeoneElse, err := NewLinuxTable(root, nil, os.Getuid()+1).Scan()
		if err != nil {
			t.Fatal(err)
		}
		if asSomeoneElse.Unreadable != 0 {
			t.Fatalf("Unreadable = %d, want 0", asSomeoneElse.Unreadable)
		}
		found := false
		for _, p := range asSomeoneElse.Procs {
			if p.PID == 22 {
				found = true
			}
		}
		if !found {
			t.Fatal("expected pid 22 in scan")
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLinuxTableUnderHidepid(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("hidepid binds only a non-root reader, and root reads a mode-000 file anyway")
	}

	t.Run("notices when other users' processes are hidden, and counts its own it cannot read rather than failing", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "self", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "self", "ns", "pid"))
		mustMkdirAll(t, filepath.Join(root, "30", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "30", "ns", "pid"))
		mustWriteFile(t, filepath.Join(root, "30", "cmdline"), "mine\x00")
		if err := os.Chmod(filepath.Join(root, "30", "cmdline"), 0o000); err != nil {
			t.Fatal(err)
		}
		uid := os.Getuid()

		// hidepid=2: not even init can be seen by a non-root reader.
		scan, err := NewLinuxTable(root, nil, uid).Scan()
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Procs) != 0 || scan.Unreadable != 1 || !scan.OthersHidden {
			t.Fatalf("got %+v", scan)
		}
		mustMkdirAll(t, filepath.Join(root, "1"))
		scan, err = NewLinuxTable(root, nil, uid).Scan()
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Procs) != 0 || scan.Unreadable != 1 || scan.OthersHidden {
			t.Fatalf("got %+v", scan)
		}
		// hidepid=1: someone else's command line is there but cannot be read.
		scan, err = NewLinuxTable(root, nil, uid+1).Scan()
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Procs) != 0 || scan.Unreadable != 0 || !scan.OthersHidden {
			t.Fatalf("got %+v", scan)
		}
	})

	t.Run("does not take a process whose owner it cannot read for its own", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "self", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "self", "ns", "pid"))
		mustMkdirAll(t, filepath.Join(root, "1"))
		// Neither its command line nor its owner can be read, and it has not gone: ELOOP, not ENOENT.
		mustSymlink(t, filepath.Join(root, "31"), filepath.Join(root, "31"))

		scan, err := NewLinuxTable(root, nil, os.Getuid()).Scan()
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Procs) != 0 || scan.Unreadable != 0 || !scan.OthersHidden {
			t.Fatalf("got %+v", scan)
		}
	})

	t.Run("judges another user's process by its command line alone: a private environment is not a blind spot", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "self", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "self", "ns", "pid"))
		mustMkdirAll(t, filepath.Join(root, "1"))
		mustMkdirAll(t, filepath.Join(root, "40", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "40", "ns", "pid"))
		mustWriteFile(t, filepath.Join(root, "40", "cmdline"), "helper\x00")
		mustMkdirAll(t, filepath.Join(root, "40", "environ")) // unreadable, even to root

		scan, err := NewLinuxTable(root, nil, os.Getuid()+1).Scan()
		if err != nil {
			t.Fatal(err)
		}
		want := []Proc{{PID: 40, Args: []string{"helper"}}}
		if len(scan.Procs) != 1 || scan.Procs[0].PID != want[0].PID || !equalStrings(scan.Procs[0].Args, want[0].Args) || scan.Procs[0].EnvRead {
			t.Fatalf("got %+v", scan.Procs)
		}
		if scan.Unreadable != 0 || scan.OthersHidden {
			t.Fatalf("got %+v", scan)
		}
	})

	t.Run("never calls root's view hidden: hidepid does not apply to it", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "self", "ns"))
		mustSymlink(t, "pid:[1]", filepath.Join(root, "self", "ns", "pid"))

		scan, err := NewLinuxTable(root, nil, 0).Scan()
		if err != nil || scan.OthersHidden {
			t.Fatalf("got %+v, err %v", scan, err)
		}
		scan, err = NewLinuxTable(root, nil, 1_000).Scan()
		if err != nil || !scan.OthersHidden {
			t.Fatalf("got %+v, err %v", scan, err)
		}
	})
}
