package lock

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestWaiterReopensAReplacedTarget is scenario 13 from the design doc: a
// waiter's open file description outlives the path it was opened for if
// something outside berth replaces the target while a lock is contended.
// Berth itself never unlinks a lock file, but Acquire must still never end
// up "holding" a detached, unlinked inode — it must notice and reopen.
func TestWaiterReopensAReplacedTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	scope := Scope{Target: target, Description: "worktree main"}

	holder := New(scope, testOptions(t))
	if _, err := holder.Acquire(context.Background(), "holder"); err != nil {
		t.Fatal(err)
	}
	beforeID := holder.fileID

	waiter := New(scope, testOptions(t))
	waiter.opts.Timeout = 3 * time.Second
	waiter.opts.Retry = 10 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		_, err := waiter.Acquire(context.Background(), "waiter")
		result <- err
	}()

	// Only once the waiter has its own descriptor on the current (soon to be
	// stale) inode: replaced any earlier, it would simply open the new file,
	// and the test would pass without exercising the reopen at all.
	deadline := time.Now().Add(3 * time.Second)
	for descriptorsOn(beforeID) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never opened the target")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newID, err := statFileID(target)
	if err != nil {
		t.Fatal(err)
	}
	if newID == beforeID {
		t.Fatal("test setup did not actually change the inode")
	}

	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("waiter.Acquire: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the waiter to acquire the replaced target")
	}

	if waiter.fileID != newID {
		t.Fatalf("waiter ended up holding %+v, want the replaced target's %+v", waiter.fileID, newID)
	}
	_ = waiter.Release()
}

// descriptorsOn counts this process's open descriptors on id: the holder's,
// and the waiter's once it has opened the target to poll it.
func descriptorsOn(id FileID) int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		if uint64(st.Dev) == id.Dev && uint64(st.Ino) == id.Ino {
			count++
		}
	}
	return count
}

func statFileID(path string) (FileID, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileID{}, err
	}
	defer f.Close()
	return fstatID(f)
}
