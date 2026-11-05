//go:build linux

package worktreestate

import "testing"

// TestReadMountsOnLinux reads the real mount table, which always lists the
// root filesystem.
func TestReadMountsOnLinux(t *testing.T) {
	mounts, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) == 0 || mounts["/"] == "" {
		t.Fatalf("mounts = %v, want a non-empty table that lists /", mounts)
	}
}
