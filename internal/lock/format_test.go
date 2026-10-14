package lock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordFormat covers the version in a lock's .holder record
// (docs/design/state.md): written with every record, a newer one is a holder
// that cannot be judged, so it is refused until `worktree unlock`, and a record
// with no version is no record.
func TestRecordFormat(t *testing.T) {
	// holderOf stages a lock with a record left behind by a holder that never
	// released, then replaces the record's text with raw.
	holderOf := func(t *testing.T, raw string) Scope {
		t.Helper()
		scope := Scope{Target: filepath.Join(t.TempDir(), "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scope.Target+".holder", []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return scope
	}
	const newer = `{"version":2,"holders":[{"pid":4242}],"anything":"else"}`

	t.Run("writes version 1 into every record", func(t *testing.T) {
		scope := Scope{Target: filepath.Join(t.TempDir(), "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer lk.Release()
		data, err := os.ReadFile(scope.Target + ".holder")
		if err != nil {
			t.Fatal(err)
		}
		var rec map[string]any
		if err := json.Unmarshal(data, &rec); err != nil || rec["version"] != float64(1) {
			t.Fatalf("record = %s, %v", data, err)
		}
	})

	t.Run("reads a record of a newer format as a holder it cannot identify, whatever else it holds", func(t *testing.T) {
		scope := holderOf(t, newer)
		rec, err := readRecord(scope.Target + ".holder")
		if err != nil || rec == nil || rec.newer != "2" || rec.PID != 0 {
			t.Fatalf("record = %+v, %v", rec, err)
		}
		verdict := mustJudge(t, rec, self, newFakeTable(self))
		want := "its holder record is format 2, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again"
		if verdict.Kind != Unverifiable || verdict.Reason != want {
			t.Fatalf("verdict = %+v", verdict)
		}
		if got := HolderLabel(rec); got != "a holder recorded in format 2 by a newer berth" {
			t.Fatalf("HolderLabel = %q", got)
		}
	})

	t.Run("refuses to take a lock whose unreleased record a newer berth wrote, naming the versions", func(t *testing.T) {
		scope := holderOf(t, newer)
		_, err := New(scope, testOptions(t)).Acquire(context.Background(), "berth up")
		want := "Not taking the worktree main lock: its holder record is format 2, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again."
		if err == nil || err.Error() != want {
			t.Fatalf("got %v\nwant %s", err, want)
		}
	})

	t.Run("refuses a lock whose newer record says it was released, since this build cannot read the release", func(t *testing.T) {
		scope := holderOf(t, `{"version":2,"releasedAt":"2026-10-14T10:00:00Z","pid":4242}`)
		_, err := New(scope, testOptions(t)).Acquire(context.Background(), "berth up")
		if err == nil || !strings.Contains(err.Error(), "its holder record is format 2, written by a newer berth") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("Clear refuses it until told the holder is unverifiable, then accepts it and says why", func(t *testing.T) {
		scope := holderOf(t, newer)
		opts := clearOptions(t)
		_, err := Clear(context.Background(), []Scope{scope}, opts)
		if err == nil || !strings.Contains(err.Error(), "written by a newer berth") || !strings.Contains(err.Error(), "--holder-unverifiable") {
			t.Fatalf("got %v", err)
		}
		opts.AllowUnverifiable = true
		result, err := Clear(context.Background(), []Scope{scope}, opts)
		if err != nil || len(result.Scopes) != 1 || !result.Scopes[0].Cleared || len(result.Scopes[0].Unverified) != 1 ||
			!strings.Contains(result.Scopes[0].Unverified[0], "format 2") {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if _, err := New(scope, testOptions(t)).Acquire(context.Background(), "berth up"); err != nil {
			t.Fatalf("the lock is still refused after Clear: %v", err)
		}
	})

	for name, raw := range map[string]string{
		"no version":   `{"pid":4242,"host":"laptop","command":"berth up","file":{"dev":1,"ino":2}}`,
		"version 0":    `{"version":0,"pid":4242,"host":"laptop","command":"berth up"}`,
		"a string":     `{"version":"2","pid":4242,"host":"laptop"}`,
		"a fractional": `{"version":1.5,"pid":4242,"host":"laptop"}`,
	} {
		t.Run("reads a record with "+name+" as no record at all, never as a newer one", func(t *testing.T) {
			scope := holderOf(t, raw)
			rec, err := readRecord(scope.Target + ".holder")
			if err != nil || rec != nil {
				t.Fatalf("record = %+v, %v", rec, err)
			}
		})
	}
}
