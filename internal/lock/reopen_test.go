package lock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/logger"
)

// TestWaiterReopensAReplacedTarget is scenario 13 from the design doc: a
// waiter's open file description outlives the path it was opened for if
// something outside treerig replaces the target while a lock is contended.
// Treerig itself never unlinks a lock file, but Acquire must still never end
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

	// The waiter notices at once, and it can only notice once it has opened
	// the current (soon to be stale) inode and found it held. Replaced any
	// earlier, it would simply open the new file, and the test would pass
	// without exercising the reopen at all.
	noticed := &logger.Recording{}
	noNoticeDelay := time.Duration(0)
	waiter := New(scope, testOptions(t))
	waiter.opts.Timeout = 10 * time.Second
	waiter.opts.Retry = 10 * time.Millisecond
	waiter.opts.NoticeDelay = &noNoticeDelay
	waiter.opts.Logger = noticed
	result := make(chan error, 1)
	go func() {
		_, err := waiter.Acquire(context.Background(), "waiter")
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lines, _ := noticed.Snapshot(); len(lines) > 0 {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("the waiter stopped waiting before the target was replaced: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiter never reported waiting on the target")
		}
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

// statFileID opens path and reads its current device and inode, for
// asserting that a lock target has (or has not) been replaced.
func statFileID(path string) (FileID, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileID{}, err
	}
	defer f.Close()
	return fstatID(f)
}
