package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/worktree"
)

// mutateRecord rewrites a lock's sidecar holder record in place.
func mutateRecord(t *testing.T, scope lock.Scope, edit func(rec map[string]any)) {
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
	edit(rec)
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mutateRecordMachineID rewrites a lock's sidecar holder record to name
// another machine, so its holder cannot be checked from this one.
func mutateRecordMachineID(t *testing.T, scope lock.Scope, machineID string) {
	t.Helper()
	mutateRecord(t, scope, func(rec map[string]any) { rec["machineId"] = machineID })
}

// deadPID is a PID no test system running this suite is expected to have —
// the same choice internal/lock's own legacy_test.go makes for the same reason.
const deadPID = 999999

func unlock(t *testing.T, tc *commandstest.TestContext, options commands.UnlockOptions) (*commands.UnlockResult, error) {
	t.Helper()
	return commands.UnlockWithContext(context.Background(), tc.CommandContext, options)
}

// killDeadHolder leaves a worktree's operation and development locks the way
// a holder killed outright would: taken, then left unreleased, naming a PID
// that does not exist. Acquire always records this test process's own real
// PID, which is still very much alive, so the lock is taken and cleanly
// released first (which gives the record a correct file identity and this
// machine's real identity) and the sidecar is then rewritten to name a dead
// one instead — this is the Go counterpart of the TypeScript suite's
// killedHolder helper, which simply invents a whole record because the
// TypeScript build never has to prove one against a flock it is still
// holding.
func killDeadHolder(t *testing.T, controlHome string, ref worktree.Ref) {
	t.Helper()
	markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
	operation, development := lock.WorktreeScopes(controlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
	for _, scope := range []lock.Scope{operation, development} {
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatalf("acquire %s: %v", scope.Description, err)
		}
		if err := lk.Release(); err != nil {
			t.Fatalf("release %s: %v", scope.Description, err)
		}
		mutateRecord(t, scope, func(rec map[string]any) {
			rec["pid"] = deadPID
			delete(rec, "releasedAt")
		})
	}
}

func TestWorktreeUnlock(t *testing.T) {
	// "is named by UUID in every mutation's stale-lock notice: a slug is
	// unique only within its domain" tests lockOptions().recovery, a
	// heartbeat-design notice that pointed at `worktree unlock <uuid>` as
	// the way to clear a stale lock TS's own ScopedLock waits on. The flock
	// design's own wait notice (internal/lock's advice/who, pinned by row
	// 3) never names a recovery command at all — a waiter is told who holds
	// the lock and how to signal it, not to run another command — so there
	// is no Go counterpart of the string this test checks.
	t.Run("is named by UUID in every mutation's stale-lock notice: a slug is unique only within its domain", func(t *testing.T) {
		t.Skip("this checks a heartbeat-design notice's own recovery hint, which named `worktree unlock <uuid>`; " +
			"the flock design's wait notice (docs/port/divergences.md, row 3) never names a recovery command at all")
	})

	t.Run("says there is nothing to unlock when nothing holds the worktree", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		result, err := unlock(t, tc, commands.UnlockOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 0 {
			t.Fatalf("cleared = %+v, want none", result.Cleared)
		}
		want := []string{"info: Nothing to unlock: no lock is held for feature-login.acme.localhost."}
		if got := tc.Logger.Lines; !equalLines(got, want) {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	})

	t.Run("refuses a live holder, naming it, and clears nothing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
		operation, _ := lock.WorktreeScopes(w.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
		holder := lock.New(operation, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()

		_, err := unlock(t, tc, commands.UnlockOptions{})
		wantErr(t, err, `^The worktree feature-login lock is held, so there is nothing to unlock\.`)
	})

	t.Run("clears the locks of a holder proven gone, and says what it checked", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		killDeadHolder(t, w.ControlHome, ref)
		tc := w.Context("", acme, nil)

		result, err := unlock(t, tc, commands.UnlockOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 2 {
			t.Fatalf("cleared = %+v, want 2", result.Cleared)
		}
		if result.Cleared[0].Kind != "development" || result.Cleared[1].Kind != "operation" {
			t.Fatalf("cleared order = %+v", result.Cleared)
		}
		lines := tc.Logger.Lines
		if len(lines) == 0 || lines[len(lines)-1] != "info: Unlocked feature-login.acme.localhost: cleared the feature-login development session lock and the worktree feature-login lock." {
			t.Fatalf("last line = %q", lines[len(lines)-1])
		}

		// The worktree's locks work again.
		operation, development := lock.WorktreeScopes(w.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, procid.Markers{})
		for _, scope := range []lock.Scope{operation, development} {
			lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
			if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatalf("acquire %s after unlock: %v", scope.Description, err)
			}
			_ = lk.Release()
		}
	})

	t.Run("names another worktree by its slug", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		other := w.Worktree("other", "feature/other")
		ref := w.Register(other, acme, "feature-other", brand.LabelNamespace, "api")
		killDeadHolder(t, w.ControlHome, ref)

		result, err := unlock(t, w.Context("", acme, nil), commands.UnlockOptions{Identifier: "feature-other"})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 2 {
			t.Fatalf("cleared = %+v, want 2", result.Cleared)
		}
	})

	t.Run("refuses a name nobody registered, and a checkout that was never brought up", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)

		_, err := unlock(t, tc, commands.UnlockOptions{Identifier: "nobody"})
		wantErr(t, err, `^No worktree is registered as nobody\.$`)

		_, err = unlock(t, tc, commands.UnlockOptions{})
		wantErr(t, err, `This checkout has never been brought up`)
	})

	t.Run("finds a lock a first up left behind before it registered, by the checkout's own markers", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		identity, err := tc.CommandContext.Registry.Identities.Ensure(context.Background(), w.Root)
		if err != nil {
			t.Fatal(err)
		}
		killDeadHolder(t, w.ControlHome, worktree.Ref{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: identity.Root})

		result, err := unlock(t, tc, commands.UnlockOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Name != identity.Root {
			t.Fatalf("name = %q, want %q", result.Name, identity.Root)
		}
		if len(result.Cleared) != 2 {
			t.Fatalf("cleared = %+v, want 2", result.Cleared)
		}
		lines := tc.Logger.Lines
		want := "info: Unlocked " + identity.Root + ": cleared the " + identity.Root + " development session lock and the worktree " + identity.Root + " lock."
		if lines[len(lines)-1] != want {
			t.Fatalf("last line = %q, want %q", lines[len(lines)-1], want)
		}
	})

	t.Run("takes the person's word for a holder it cannot check only when told to, and says so", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		killDeadHolder(t, w.ControlHome, ref)

		markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
		operation, development := lock.WorktreeScopes(w.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
		for _, scope := range []lock.Scope{operation, development} {
			mutateRecordMachineID(t, scope, "another-machine")
		}

		tc := w.Context("", acme, nil)
		_, err := unlock(t, tc, commands.UnlockOptions{})
		wantErr(t, err, `ran on another machine`)

		tc2 := w.Context("", acme, nil)
		result, err := unlock(t, tc2, commands.UnlockOptions{AllowUnverifiable: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 2 {
			t.Fatalf("cleared = %+v, want 2", result.Cleared)
		}
		warnings := 0
		for _, line := range tc2.Logger.Lines {
			if hasPrefix(line, "warn: Not verified, on your word (--holder-unverifiable):") {
				warnings++
			}
		}
		if warnings != 2 {
			t.Fatalf("warnings = %d, want 2", warnings)
		}
	})

	t.Run("reaches a worktree that never registered, and whose checkout is gone, by its UUID", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		ref := worktree.Ref{RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676", WorktreeID: "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"}
		killDeadHolder(t, w.ControlHome, ref)

		result, err := unlock(t, tc, commands.UnlockOptions{Identifier: ref.WorktreeID})
		if err != nil {
			t.Fatal(err)
		}
		if result.Name != ref.WorktreeID || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v", result)
		}

		_, err = unlock(t, tc, commands.UnlockOptions{Identifier: "ffffffff-ffff-4fff-8fff-ffffffffffff"})
		wantErr(t, err, `No worktree is registered as ffffffff-ffff-4fff-8fff-ffffffffffff, and no lock is held for a worktree of that UUID\.`)
	})

	// "refuses when the marker turns up during the second look, clearing
	// nothing" exercises unlockGuard's ~12 second second look: it judges a
	// holder gone, waits, and judges again before trusting that verdict, so
	// a lock reclaimed in between is still caught. lock.Clear has no second
	// look to race: flock(2) is held for the judging, marking-released and
	// legacy-leftover cleanup in one call, so nothing else can reclaim the
	// lock in between (docs/port/divergences.md, row 19).
	t.Run("refuses when the marker turns up during the second look, clearing nothing", func(t *testing.T) {
		t.Skip("unlockGuard's ~12 second second look — and the race this test wins against it — has no Go counterpart: " +
			"lock.Clear holds the flock for the whole judge-then-clear sequence, so nothing can reclaim the lock in between " +
			"(docs/port/divergences.md, row 19)")
	})
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
