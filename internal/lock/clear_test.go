package lock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/procid"
)

// clearOptions returns ClearOptions wired to a fake process table that
// reports the local machine as the caller's own, for tests that call Clear
// directly.
func clearOptions(t *testing.T) ClearOptions {
	t.Helper()
	return ClearOptions{Table: newFakeTable(procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"}), SuggestUnverifiableFlag: true}
}

// mutateRecordField rewrites one field of a lock's sidecar holder record in place.
func mutateRecordField(t *testing.T, scope Scope, field string, value any) {
	t.Helper()
	path := scope.Target + ".holder"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec[field] = value
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClear(t *testing.T) {
	t.Run("does nothing to a target that was never created", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		result, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 0 {
			t.Fatalf("result = %+v, want no scopes at all", result)
		}
		if _, statErr := os.Stat(scope.Target); !os.IsNotExist(statErr) {
			t.Fatalf("Clear must not create the target on a missing lock, stat err = %v", statErr)
		}
	})

	t.Run("does nothing to a lock that was taken and released cleanly", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		result, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 1 || result.Scopes[0].Cleared {
			t.Fatalf("result = %+v, want one scope, Cleared false", result)
		}
		if result.Proof != "" {
			t.Fatalf("proof = %q, want none: nothing was cleared", result.Proof)
		}
	})

	t.Run("does not scan for survivors when every lock is already released", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main", Markers: procid.Markers{Project: "berth-released"}}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}

		// Either of these would refuse, were the scan made: a marked process
		// still running, and other users' processes hidden.
		opts := clearOptions(t)
		table := opts.Table.(*fakeTable)
		table.scans = []procid.Scan{{Procs: []procid.Proc{{PID: 4242, Args: []string{"berth-released"}}}, OthersHidden: true}}

		result, err := Clear(context.Background(), []Scope{scope}, opts)
		if err != nil {
			t.Fatalf("err = %v, want none: there was nothing to clear", err)
		}
		if len(result.Scopes) != 1 || result.Scopes[0].Cleared {
			t.Fatalf("result = %+v, want one scope, Cleared false", result)
		}
		if table.scanned != 0 {
			t.Fatalf("scanned %d times, want none", table.scanned)
		}
	})

	// Pins #21's rewording: the survivor-scan refusal used to say "for this
	// worktree" unconditionally, which was wrong the moment a scope with no
	// worktree at all (the router lock) could reach it; it now says "it
	// started" instead, referring back to "the holder" the caller's own
	// "Checking that the holder of …'s lock…" notice just named, rather
	// than the jargon "marked". Real time (judgeDelay's rescan): about a second.
	t.Run("refuses a process it started still running, without claiming it belongs to a worktree", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "router"), Description: "shared router", Markers: procid.Markers{Project: "berth-router"}}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}

		opts := clearOptions(t)
		table := opts.Table.(*fakeTable)
		table.scans = []procid.Scan{{Procs: []procid.Proc{{PID: 4242, Args: []string{"berth-router"}}}}}

		_, err := Clear(context.Background(), []Scope{scope}, opts)
		want := "Not clearing any lock: a process it started is still running"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want a refusal containing %q", err, want)
		}
		if strings.Contains(err.Error(), "worktree") || strings.Contains(err.Error(), "marked") {
			t.Fatalf("err = %v, want no mention of a worktree or the jargon \"marked\": this scope is not one", err)
		}
	})

	t.Run("refuses a lock that is actively held", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer lk.Release()

		_, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		if err == nil || !strings.Contains(err.Error(), "The worktree main lock is held, so there is nothing to unlock.") {
			t.Fatalf("err = %v, want a held refusal", err)
		}
	})

	t.Run("clears a lock whose holder is judged gone, and marks its record released", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main", Markers: procid.Markers{Project: "berth-abc"}}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil { // leaves the record unreleased, as a killed holder would
			t.Fatal(err)
		}

		result, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 1 || !result.Scopes[0].Cleared || len(result.Scopes[0].Verified) != 1 || len(result.Scopes[0].Unverified) != 0 {
			t.Fatalf("result = %+v", result)
		}
		if result.Proof != "no command it started is still running on this machine" {
			t.Fatalf("proof = %q, want the survivor-scan wording (#21)", result.Proof)
		}
		rec, err := readRecord(scope.Target + ".holder")
		if err != nil || rec == nil || rec.ReleasedAt == nil {
			t.Fatalf("record = %+v, err = %v, want it marked released", rec, err)
		}

		// The flock itself is free again: another acquirer does not wait.
		again := New(scope, testOptions(t))
		if _, err := again.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatalf("expected the flock to be free after Clear: %v", err)
		}
		_ = again.Release()
	})

	// Pins #21: the registry lock (and any other scope with no markers of
	// its own) is never scanned — it guards only brief writes, with no
	// process of its own to check for — so its proof says so honestly
	// rather than claiming a scan that never ran, and never mentions
	// "worktree", since this scope is not one.
	t.Run("clears a lock with no markers of its own, and says so honestly rather than claiming a scan", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "registry"), Description: "global worktree registry"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}

		opts := clearOptions(t)
		table := opts.Table.(*fakeTable)
		result, err := Clear(context.Background(), []Scope{scope}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 1 || !result.Scopes[0].Cleared {
			t.Fatalf("result = %+v", result)
		}
		if want := "it holds no process markers of its own, so there is nothing further to check"; result.Proof != want {
			t.Fatalf("proof = %q, want %q", result.Proof, want)
		}
		if table.scanned != 0 {
			t.Fatalf("scanned %d times, want none: a markerless scope has nothing to scan for", table.scanned)
		}
	})

	t.Run("refuses an unverifiable holder unless allowed, and accepts it when told to", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}
		// A record naming another machine cannot be checked at all.
		rec, err := readRecord(scope.Target + ".holder")
		if err != nil || rec == nil {
			t.Fatalf("record = %+v, err = %v", rec, err)
		}
		rec.MachineID = "elsewhere"
		if err := writeRecordAtomic(scope.Target+".holder", *rec); err != nil {
			t.Fatal(err)
		}

		opts := clearOptions(t)
		_, err = Clear(context.Background(), []Scope{scope}, opts)
		if err == nil || !strings.Contains(err.Error(), "ran on another machine") || !strings.Contains(err.Error(), "--holder-unverifiable") {
			t.Fatalf("err = %v, want an unverifiable refusal naming the flag", err)
		}

		opts.AllowUnverifiable = true
		result, err := Clear(context.Background(), []Scope{scope}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 1 || !result.Scopes[0].Cleared || len(result.Scopes[0].Unverified) != 1 || len(result.Scopes[0].Verified) != 0 {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("the --holder-unverifiable advice appears only when SuggestUnverifiableFlag is set", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}
		mutateRecordField(t, scope, "machineId", "elsewhere")

		opts := clearOptions(t)
		opts.SuggestUnverifiableFlag = false // the registry and router locks: no worktree unlock flag to offer
		_, err := Clear(context.Background(), []Scope{scope}, opts)
		if err == nil || !strings.Contains(err.Error(), "ran on another machine") {
			t.Fatalf("err = %v, want an unverifiable refusal", err)
		}
		if strings.Contains(err.Error(), "--holder-unverifiable") {
			t.Fatalf("err = %v, want no --holder-unverifiable advice for a non-worktree scope", err)
		}
	})

	t.Run("refuses a TypeScript-era lock directory whose target was never written, with the rmdir that clears it", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		if err := os.Mkdir(scope.Target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		want := "Not clearing the worktree main lock: " + scope.Target + ".lock exists but names no holder"
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "rmdir '"+scope.Target+".lock'") {
			t.Fatalf("err = %v, want a refusal containing %q and the rmdir that clears it", err, want)
		}
		if _, statErr := os.Lstat(scope.Target + ".lock"); statErr != nil {
			t.Fatalf("the .lock directory must be left alone: %v", statErr)
		}
	})

	t.Run("clears a TypeScript-era leftover with no sidecar record of its own", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := `{"pid":999999,"host":"old-host","command":"berth up","startedAt":"2020-01-01T00:00:00.000Z","machineId":"m","bootId":"b","pidNs":"pid:[1]"}`
		if err := os.WriteFile(target, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}

		scope := Scope{Target: target, Description: "worktree main"}
		result, err := Clear(context.Background(), []Scope{scope}, clearOptions(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Scopes) != 1 || !result.Scopes[0].Cleared || len(result.Scopes[0].Verified) != 1 {
			t.Fatalf("result = %+v", result)
		}
		if _, statErr := os.Lstat(target + ".lock"); !os.IsNotExist(statErr) {
			t.Fatalf("expected the .lock directory to be gone, stat err = %v", statErr)
		}
	})

	// Ported from tests/unit/operation-lock.test.ts:155 ("preserves a stale
	// operation lock when its recovery guard fails"): TypeScript's
	// clearStaleWorktreeLocks verifies every stale lock together before
	// clearing any of them, so a guard failure on one leaves every other
	// exactly as it was. Go's own guard is judging itself, not an injectable
	// callback, so this drives the failure through a real judging refusal
	// (an unverifiable holder) on one scope while the other is an ordinary
	// dead holder, ready to clear — and confirms neither one commits.
	t.Run("preserves a stale operation lock when its recovery guard fails", func(t *testing.T) {
		dir := t.TempDir()
		goneScope := Scope{Target: filepath.Join(dir, "development"), Description: "worktree main development session"}
		operationScope := Scope{Target: filepath.Join(dir, "operation"), Description: "worktree main"}

		for _, scope := range []Scope{goneScope, operationScope} {
			lk := New(scope, testOptions(t))
			if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatal(err)
			}
			if err := lk.Abandon(); err != nil {
				t.Fatal(err)
			}
		}
		// The operation lock's holder cannot be checked at all: its guard fails.
		mutateRecordField(t, operationScope, "machineId", "elsewhere")

		_, err := Clear(context.Background(), []Scope{goneScope, operationScope}, clearOptions(t))
		if err == nil || !strings.Contains(err.Error(), "ran on another machine") {
			t.Fatalf("err = %v, want the operation lock's guard failure", err)
		}

		for _, scope := range []Scope{goneScope, operationScope} {
			rec, err := readRecord(scope.Target + ".holder")
			if err != nil || rec == nil || rec.ReleasedAt != nil {
				t.Fatalf("%s: record = %+v, err = %v, want it left unreleased", scope.Description, rec, err)
			}
		}
		// The flock itself is free again — Clear released it on the way
		// out — proven on goneScope, whose record does not also block a
		// fresh Acquire from judging it (unlike operationScope's, which
		// still names an unverifiable holder and so refuses Acquire too,
		// exactly as it refused Clear: nothing here silently overwrote it).
		probe := New(goneScope, testOptions(t))
		if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
			t.Fatalf("expected the flock to be free (just uncommitted), got %v", err)
		}
		_ = probe.Abandon()
	})

	// The asymmetric case the review specifically asked for: one scope's
	// holder judges gone, the other's judges unverifiable — the whole batch
	// still refuses, and neither one clears.
	t.Run("refuses the whole batch when one scope is gone and the other unverifiable, clearing neither", func(t *testing.T) {
		dir := t.TempDir()
		goneScope := Scope{Target: filepath.Join(dir, "development"), Description: "worktree main development session"}
		unverifiableScope := Scope{Target: filepath.Join(dir, "operation"), Description: "worktree main"}

		for _, scope := range []Scope{goneScope, unverifiableScope} {
			lk := New(scope, testOptions(t))
			if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatal(err)
			}
			if err := lk.Abandon(); err != nil {
				t.Fatal(err)
			}
		}
		mutateRecordField(t, unverifiableScope, "machineId", "elsewhere")

		result, err := Clear(context.Background(), []Scope{goneScope, unverifiableScope}, clearOptions(t))
		if err == nil {
			t.Fatalf("expected a refusal, got result = %+v", result)
		}
		if !strings.Contains(err.Error(), "ran on another machine") {
			t.Fatalf("err = %v", err)
		}
		for _, scope := range []Scope{goneScope, unverifiableScope} {
			rec, err := readRecord(scope.Target + ".holder")
			if err != nil || rec == nil || rec.ReleasedAt != nil {
				t.Fatalf("%s: expected it left unreleased, record = %+v, err = %v", scope.Description, rec, err)
			}
		}
	})

	// Phase 2 (commit) itself can fail partway through a batch phase 1 has
	// already judged clean entirely — a write failing after an earlier
	// scope in the same batch already committed, say. Clear's own doc
	// comment promises every ScopeResult decided so far in that case,
	// rather than discarding what was actually cleared; internal/commands's
	// UnlockWithContext is what turns that into the "Already cleared..."
	// sentence a person sees.
	t.Run("reports what it already cleared when a later commit in the same batch fails", func(t *testing.T) {
		skipIfRoot(t)
		firstDir := t.TempDir()
		secondDir := t.TempDir()
		firstScope := Scope{Target: filepath.Join(firstDir, "development"), Description: "worktree main development session"}
		secondScope := Scope{Target: filepath.Join(secondDir, "operation"), Description: "worktree main"}

		for _, scope := range []Scope{firstScope, secondScope} {
			lk := New(scope, testOptions(t))
			if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatal(err)
			}
			if err := lk.Abandon(); err != nil {
				t.Fatal(err)
			}
		}

		// Phase 1 (judging) only reads, so this changes nothing it sees;
		// writeRecordAtomic creates its temp file in the record's own
		// directory, so taking away secondDir's write permission blocks
		// only secondScope's phase-2 commit, once firstScope's own commit
		// (in a different directory) has already gone through.
		if err := os.Chmod(secondDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(secondDir, 0o700) })

		result, err := Clear(context.Background(), []Scope{firstScope, secondScope}, clearOptions(t))
		if err == nil {
			t.Fatalf("expected the second scope's commit to fail, got result = %+v", result)
		}
		if len(result.Scopes) != 2 || !result.Scopes[0].Cleared || result.Scopes[1].Cleared {
			t.Fatalf("result = %+v, want the first scope reported cleared and the second not", result)
		}
		firstRec, err := readRecord(firstScope.Target + ".holder")
		if err != nil || firstRec == nil || firstRec.ReleasedAt == nil {
			t.Fatalf("first record = %+v, err = %v, want it marked released", firstRec, err)
		}
		secondRec, err := readRecord(secondScope.Target + ".holder")
		if err != nil || secondRec == nil || secondRec.ReleasedAt != nil {
			t.Fatalf("second record = %+v, err = %v, want it left unreleased", secondRec, err)
		}
	})

	t.Run("ends a contradiction's second look early on cancellation", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		opts := clearOptions(t)
		table := opts.Table.(*fakeTable)
		lk := New(scope, Options{Table: table, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}
		// Reported running: with a matching identity and a free flock, this
		// is the contradiction judgeForClear takes a second look at.
		table.states[os.Getpid()] = procid.Status{State: procid.Running}

		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
		start := time.Now()
		_, err := Clear(ctx, []Scope{scope}, opts)
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("took %s, want the second look to end on cancellation instead of waiting it out", time.Since(start))
		}
		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) {
			t.Fatalf("err = %v, want *clierr.Cancelled", err)
		}
	})

	t.Run("ends a survivor rescan early on cancellation", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{
			Target: filepath.Join(dir, "worktree"), Description: "worktree main",
			Markers: procid.Markers{Project: "berth-cancel"},
		}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}

		opts := clearOptions(t)
		table := opts.Table.(*fakeTable)
		// A survivor on the first scan is what makes scanSurvivorsAcross
		// take its own second look, exactly like judgeForClear's above.
		table.scans = []procid.Scan{{Procs: []procid.Proc{{PID: 4242, Args: []string{"berth-cancel"}}}}}

		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
		start := time.Now()
		_, err := Clear(ctx, []Scope{scope}, opts)
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("took %s, want the rescan to end on cancellation instead of waiting it out", time.Since(start))
		}
		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) {
			t.Fatalf("err = %v, want *clierr.Cancelled", err)
		}
	})
}
