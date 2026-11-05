package worktreestate_test

import (
	"testing"

	"github.com/sofired/treerig/internal/worktreestate"
)

func TestMounts(t *testing.T) {
	const table = `22 28 0:21 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
28 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw,errors=remount-ro
40 28 0:35 / /mnt/with\040space rw,relatime shared:25 - ext4 /dev/sdb1 rw
41 28 0:36 / /net rw,relatime shared:26 - autofs systemd-1 rw,fd=30,pgrp=1,timeout=0,minproto=5,maxproto=5,direct
`
	mounts, err := worktreestate.ParseMountInfo(table)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reads each mount point and its filesystem type, unescaping the path", func(t *testing.T) {
		for point, fstype := range map[string]string{"/sys": "sysfs", "/": "ext4", "/mnt/with space": "ext4", "/net": "autofs"} {
			if got := mounts[point]; got != fstype {
				t.Errorf("%q = %q, want %q", point, got, fstype)
			}
		}
		if len(mounts) != 4 {
			t.Errorf("mounts = %v, want the four mounts", mounts)
		}
	})

	t.Run("fails closed on a line it cannot read, rather than skipping a mount it may need", func(t *testing.T) {
		for _, bad := range []string{"this line is not a mount\n", "22 28 0:21 / /sys rw - \n", table + "garbage\n"} {
			if got, err := worktreestate.ParseMountInfo(bad); err == nil || got != nil {
				t.Errorf("%q: got %v, %v; want an error and no table", bad, got, err)
			}
		}
		if got, err := worktreestate.ParseMountInfo("\n\n"); err != nil || len(got) != 0 {
			t.Errorf("blank lines are not unreadable lines: %v, %v", got, err)
		}
	})

	t.Run("knows a path at or below an automount placeholder", func(t *testing.T) {
		for path, want := range map[string]bool{"/net": true, "/net/host/export": true, "/netx": false, "/mnt/with space/src": false, "/": false} {
			if got := mounts.Automounted(path); got != want {
				t.Errorf("Automounted(%q) = %v, want %v", path, got, want)
			}
		}
	})
}
