//go:build linux

package procid

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLinuxTableHolding(t *testing.T) {
	t.Run("lists the other processes with a descriptor on the lock file, and what could be read of each", func(t *testing.T) {
		root := fakeProc(t)
		lockFile := filepath.Join(t.TempDir(), "worktree")
		unrelated := filepath.Join(t.TempDir(), "other")
		mustWriteFile(t, lockFile, "")
		mustWriteFile(t, unrelated, "")
		var st syscall.Stat_t
		if err := syscall.Stat(lockFile, &st); err != nil {
			t.Fatal(err)
		}

		// A /proc/<pid>/fd entry is a symlink to the open file: Holding stats through it.
		withFD := func(pid int, fields procFields, targets ...string) {
			addProcess(t, root, pid, fields)
			mustMkdirAll(t, filepath.Join(root, itoaInt(pid), "fd"))
			for i, target := range targets {
				mustSymlink(t, target, filepath.Join(root, itoaInt(pid), "fd", itoaInt(i+3)))
			}
		}
		withFD(101, procFields{cmdline: []string{"docker", "compose", "up"}, environ: []string{"TREERIG_WORKTREE_ID=w"}, hasEnviron: true}, unrelated, lockFile)
		withFD(102, procFields{cmdline: []string{"sleep", "30"}, environUnreadable: true}, lockFile)
		withFD(103, procFields{cmdline: []string{"vim"}, hasEnviron: true}, unrelated)
		withFD(os.Getpid(), procFields{cmdline: []string{"treerig"}, hasEnviron: true}, lockFile)
		mustMkdirAll(t, filepath.Join(root, "104")) // no fd directory: gone, or not ours to see

		procs, err := NewLinuxTable(root, nil, os.Getuid()).Holding(uint64(st.Dev), st.Ino)
		if err != nil {
			t.Fatal(err)
		}
		if len(procs) != 2 {
			t.Fatalf("got %+v, want 101 and 102 only (never this process, nor one holding something else)", procs)
		}
		byPID := map[int]Proc{procs[0].PID: procs[0], procs[1].PID: procs[1]}
		if p := byPID[101]; !p.EnvRead || !equalStrings(p.Args, []string{"docker", "compose", "up"}) || !equalStrings(p.Env, []string{"TREERIG_WORKTREE_ID=w"}) {
			t.Fatalf("101 = %+v", p)
		}
		if p := byPID[102]; p.EnvRead || !equalStrings(p.Args, []string{"sleep", "30"}) {
			t.Fatalf("102 = %+v, want its arguments with its environment marked unread", p)
		}
	})
}
