//go:build linux && integration

package worktreestate

import "testing"

// TestReadMountsOnLinux is an integration smoke test, not a unit test: it reads
// the machine's real mount table, which always lists the root filesystem. The
// parser is covered by fixtures (mounts_test.go).
func TestReadMountsOnLinux(t *testing.T) {
	mounts, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) == 0 || mounts["/"] == "" {
		t.Fatalf("mounts = %v, want a non-empty table that lists /", mounts)
	}
}
