package lock

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
)

// The wording that replaced the heartbeat design's own. Each case gives up
// on a lock held by this same process, so the record names this process
// and the fake table decides what it looks like.
func TestLockNoticeWording(t *testing.T) {
	contend := func(t *testing.T, table *fakeTable) error {
		t.Helper()
		scope := Scope{Target: filepath.Join(t.TempDir(), "worktree"), Description: "worktree busy"}
		holder := New(scope, testOptions(t))
		if _, err := holder.Acquire(context.Background(), "berth up api"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = holder.Release() })
		opts := testOptions(t)
		opts.Table = table
		opts.Timeout = 100 * time.Millisecond
		opts.Retry = 25 * time.Millisecond
		delay := time.Second
		opts.NoticeDelay = &delay
		_, err := New(scope, opts).Acquire(context.Background(), "waiter")
		if err == nil {
			t.Fatal("expected the wait to give up")
		}
		return err
	}

	t.Run("lists the processes holding the lock, where the platform can say", func(t *testing.T) {
		table := newFakeTable(procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"})
		table.holding = []procid.Proc{{PID: 77, Args: []string{"docker", "compose", "up", "--wait"}}}
		err := contend(t, table)
		if want := "\nProcesses holding it:\n  PID 77: docker compose up --wait"; !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("err = %q, want it to end with %q", err, want)
		}
	})

	t.Run("tells a stopped holder how to resume it", func(t *testing.T) {
		table := newFakeTable(procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"})
		table.states[os.Getpid()] = procid.Status{State: procid.Stopped}
		err := contend(t, table)
		want := "it is stopped, not gone: resume it with `kill -CONT " + strconv.Itoa(os.Getpid()) + "` and let it finish, or end it, then retry."
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	})

	t.Run("tells a running holder how to stop it", func(t *testing.T) {
		table := newFakeTable(procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"})
		table.states[os.Getpid()] = procid.Status{State: procid.Running}
		err := contend(t, table)
		want := "it still holds the lock. Wait for it to finish, or stop it (`kill " + strconv.Itoa(os.Getpid()) + "`), then retry."
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	})

	t.Run("warns once that a machine with no readable identity cannot recover its locks", func(t *testing.T) {
		warnUnidentified = sync.Once{}
		t.Cleanup(func() { warnUnidentified = sync.Once{} })
		recorded := &logger.Recording{}
		for i := 0; i < 2; i++ {
			opts := testOptions(t)
			opts.Table = newFakeTable(procid.Identity{})
			opts.Logger = recorded
			lk := New(Scope{Target: filepath.Join(t.TempDir(), "worktree"), Description: "worktree main"}, opts)
			if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatal(err)
			}
			_ = lk.Release()
		}
		lines, _ := recorded.Snapshot()
		want := "warn: This machine's identity could not be read (no machine ID and no boot ID), " +
			"so if this command dies holding a lock, nothing can later prove it gone and the lock will need clearing by hand."
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("logged %q, want exactly one %q", lines, want)
		}
	})
}
