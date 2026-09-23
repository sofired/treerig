package lock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/berth/internal/procid"
)

func TestRecoverLegacyLeftover(t *testing.T) {
	t.Run("clears a TypeScript-era lock directory once its holder is judged gone", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := map[string]any{
			"pid": 999999, "host": "old-host", "command": "berth up",
			"startedAt": time.Now().Format(time.RFC3339Nano),
			"machineId": "m", "bootId": "b", "pidNs": "pid:[1]",
		}
		data, _ := json.Marshal(legacy)
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}

		scope := Scope{Target: target, Description: "worktree main"}
		lk := New(scope, testOptions(t)) // fakeTable's identity is {MachineID:"m", BootID:"b", PIDNS:"pid:[1]"}; PID 999999 is absent
		ctx, err := lk.Acquire(context.Background(), "berth up")
		if err != nil {
			t.Fatalf("expected the legacy leftover to be cleared, got %v", err)
		}
		defer lk.Release()
		_ = ctx

		if _, err := os.Lstat(target + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("expected the .lock directory to be gone, stat err = %v", err)
		}
	})

	t.Run("refuses when the legacy record's holder still looks alive", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		if err := os.Mkdir(target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := map[string]any{
			"pid": 4242, "host": "laptop", "command": "berth up",
			"startedAt": time.Now().Format(time.RFC3339Nano),
			"machineId": "m", "bootId": "b", "pidNs": "pid:[1]", "startTicks": 900,
		}
		data, _ := json.Marshal(legacy)
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}

		opts := testOptions(t)
		table := opts.Table.(*fakeTable)
		table.identity = procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"}
		table.states[4242] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(900))}
		scope := Scope{Target: target, Description: "worktree main"}
		lk := New(scope, opts)
		_, err := lk.Acquire(context.Background(), "berth up")
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if _, statErr := os.Lstat(target + ".lock"); statErr != nil {
			t.Fatalf("expected the .lock directory to survive a refusal, got %v", statErr)
		}
	})
}
