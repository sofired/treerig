package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/worktree"
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

// deadPID is a PID no test system running this suite is expected to have.
const deadPID = 999999

// unlock runs commands.UnlockWithContext against tc's own CommandContext.
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
// one instead, rather than simply inventing a whole record: this has to
// prove one against a flock it is still holding.
func killDeadHolder(t *testing.T, controlHome string, ref worktree.Ref) {
	t.Helper()
	markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
	operation, development := lock.WorktreeScopes(controlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
	for _, scope := range []lock.Scope{operation, development} {
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "treerig up"); err != nil {
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
	// design's own wait notice (internal/lock's advice/who) never names a
	// recovery command at all — a waiter is told who holds the lock and
	// how to signal it, not to run another command — so there is no Go
	// counterpart of the string this test checks.
	t.Run("is named by UUID in every mutation's stale-lock notice: a slug is unique only within its domain", func(t *testing.T) {
		t.Skip("this checks a heartbeat-design notice's own recovery hint, which named `worktree unlock <uuid>`; " +
			"the flock design's wait notice never names a recovery command at all")
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
		if _, err := holder.Acquire(context.Background(), "treerig up"); err != nil {
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
			if _, err := lk.Acquire(context.Background(), "treerig up"); err != nil {
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
		wantErr(t, err, `This worktree has never been brought up`)
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

	t.Run("reaches an unregistered worktree named by its UUID in upper case, clearing the lock its lowercase file holds", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		ref := worktree.Ref{RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676", WorktreeID: "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"}
		killDeadHolder(t, w.ControlHome, ref)

		result, err := unlock(t, tc, commands.UnlockOptions{Identifier: strings.ToUpper(ref.WorktreeID)})
		if err != nil {
			t.Fatal(err)
		}
		if result.Name != ref.WorktreeID || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v", result)
		}
	})

	// This scenario does not apply to Go: lock.Clear holds the flock for the
	// whole judge-then-clear sequence in one call, so nothing else can
	// reclaim the lock in between, and there is no second look to race.
	t.Run("refuses when the marker turns up during the second look, clearing nothing", func(t *testing.T) {
		t.Skip("lock.Clear holds the flock for the whole judge-then-clear sequence, so nothing can reclaim the lock in between")
	})
}

// establishControlHome leaves controlHome exactly as `up` (via
// commandstest.World.Register) or any other real mutation would before ever
// touching the registry lock: with its `.treerig-control-home` marker, so
// UnlockGlobal's own guard (finding 1 of round 2 of the four-role review of
// #21, `lstatExists` then `files.SecureControlDirectories`) recognises it
// as treerig's own rather than a foreign directory nothing created.
func establishControlHome(t *testing.T, controlHome string) {
	t.Helper()
	if err := files.SecureControlDirectories(layout.ControlLayout(controlHome), registrytest.TempDir(t)); err != nil {
		t.Fatal(err)
	}
}

// killDeadHolderScope leaves an arbitrary lock scope taken and then
// unreleased, naming a PID no test system is expected to have — the
// registry/router counterpart of killDeadHolder, which only knows how to
// build a worktree's own two scopes.
func killDeadHolderScope(t *testing.T, scope lock.Scope) {
	t.Helper()
	lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := lk.Acquire(context.Background(), "treerig up"); err != nil {
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

// fakeScanTable is a real procid.Table for Self/Inspect/Holding (so a dead
// holder's identity checks judge exactly as they would on this machine),
// with Scan scripted instead of ever reading this machine's real process
// table. Without this, TestUnlockGlobal's router case would scan the real
// host for a "treerig-router" argv token — flaky on a dev machine with a live
// router (finding 2, four-role review of #21) — and could never exercise
// the refusal branch a live survivor takes.
type fakeScanTable struct {
	procid.Table
	scan    procid.Scan
	scanned int
}

// Scan counts the call and returns f.scan, the scripted result, rather than
// reading this machine's real process table.
func (f *fakeScanTable) Scan() (procid.Scan, error) {
	f.scanned++
	return f.scan, nil
}

// globalUnlockEnv builds a *commands.Env for UnlockGlobal, which reads Env
// directly and needs no project config (see Unlock's own doc comment): a
// fresh, fake HOME per test, so the registry lock (under TREERIG_CONTROL_HOME,
// set directly to controlHome per #80) and the router lock (under
// HOME/.treerig/router, via layout.RouterState) both land in this test's own
// temp directory rather than a real one. table is a *fakeScanTable wrapping
// procid.Platform(), returned so a test can script what a survivor scan finds.
func globalUnlockEnv(t *testing.T) (env *commands.Env, controlHome string, table *fakeScanTable) {
	t.Helper()
	home := registrytest.TempDir(t)
	controlHome = filepath.Join(home, "control")
	table = &fakeScanTable{Table: procid.Platform()}
	return &commands.Env{
		CommandName: "treerig", Invocation: "treerig worktree unlock",
		Logger: &logger.Recording{},
		Table:  table,
		Getenv: func(key string) (string, bool) {
			switch key {
			case "HOME":
				return home, true
			case "TREERIG_CONTROL_HOME":
				return controlHome, true
			}
			return "", false
		},
	}, controlHome, table
}

// TestUnlockGlobal covers `worktree unlock --registry` and `--router`
// (#21): the registry and router locks are machine-wide, not a worktree's,
// so UnlockGlobal is exercised directly against a fake Env rather than
// through commandstest.World's CommandContext.
func TestUnlockGlobal(t *testing.T) {
	ctx := context.Background()

	t.Run("says there is nothing to unlock when the registry lock is not held", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 0 {
			t.Fatalf("cleared = %+v, want none", result.Cleared)
		}
		rec := env.Logger.(*logger.Recording)
		want := []string{"info: Nothing to unlock: no lock is held for the global worktree registry."}
		if !equalLines(rec.Lines, want) {
			t.Fatalf("lines = %v, want %v", rec.Lines, want)
		}
	})

	t.Run("clears a dead holder's registry lock, with no scan and no rm/rmdir advice", func(t *testing.T) {
		env, controlHome, _ := globalUnlockEnv(t)
		establishControlHome(t, controlHome)
		registryScope := lock.RegistryScope(controlHome)
		killDeadHolderScope(t, registryScope)

		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 1 || result.Cleared[0].Kind != "registry" {
			t.Fatalf("cleared = %+v", result.Cleared)
		}
		rec := env.Logger.(*logger.Recording)
		var unlocked bool
		for _, line := range rec.Lines {
			if strings.Contains(line, "rm '") || strings.Contains(line, "rmdir") {
				t.Fatalf("line %q mentions rm/rmdir advice", line)
			}
			if line == "info: Unlocked the global worktree registry: cleared the global worktree registry lock." {
				unlocked = true
			}
		}
		if !unlocked {
			t.Fatalf("lines = %v, want the Unlocked summary", rec.Lines)
		}
	})

	t.Run("clears a dead holder's router lock, scanning for anything its Compose project left running", func(t *testing.T) {
		env, _, table := globalUnlockEnv(t)
		state, err := layout.RouterState(env.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		routerScope := lock.Scope{Target: state.LockPath, Description: "shared router", Markers: procid.Markers{Project: "treerig-router"}}
		if err := os.MkdirAll(filepath.Dir(routerScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		killDeadHolderScope(t, routerScope)

		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 1 || result.Cleared[0].Kind != "router" {
			t.Fatalf("cleared = %+v", result.Cleared)
		}
		if table.scanned == 0 {
			t.Fatal("want the router's own Compose project scanned for survivors, unlike the registry")
		}
	})

	// Pins finding 2 of the four-role review of #21: without a fake table,
	// this exercised only the clean path, against this machine's real
	// process table, and could never take the refusal branch at all.
	t.Run("refuses a live router-lock survivor, and clears nothing", func(t *testing.T) {
		env, _, table := globalUnlockEnv(t)
		state, err := layout.RouterState(env.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		routerScope := lock.Scope{Target: state.LockPath, Description: "shared router", Markers: procid.Markers{Project: "treerig-router"}}
		if err := os.MkdirAll(filepath.Dir(routerScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		killDeadHolderScope(t, routerScope)
		table.scan = procid.Scan{Procs: []procid.Proc{{PID: 4242, Args: []string{"treerig-router"}}}}

		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true})
		wantErr(t, err, `a process it started is still running`)
		if result != nil && len(result.Cleared) != 0 {
			t.Fatalf("cleared = %+v, want none: a survivor refuses the whole batch", result.Cleared)
		}
	})

	t.Run("refuses --registry and --router together", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true, Router: true})
		wantErr(t, err, `pass exactly one of --registry or --router`)
	})

	t.Run("refuses --registry combined with a worktree identifier", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true, Identifier: "feature-login"})
		wantErr(t, err, `--registry clears a single machine-wide lock`)
	})

	t.Run("refuses --registry combined with --domain", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true, Domain: "example.localhost"})
		wantErr(t, err, `--registry clears a single machine-wide lock`)
	})

	t.Run("refuses --router combined with --domain", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true, Domain: "example.localhost"})
		wantErr(t, err, `--router clears a single machine-wide lock`)
	})

	t.Run("refuses a live registry lock holder, and clears nothing", func(t *testing.T) {
		env, controlHome, _ := globalUnlockEnv(t)
		establishControlHome(t, controlHome)
		registryScope := lock.RegistryScope(controlHome)
		lk := lock.New(registryScope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(ctx, "treerig up"); err != nil {
			t.Fatal(err)
		}
		defer lk.Release()

		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true})
		wantErr(t, err, `there is nothing to unlock`)
	})

	// Pins finding 7 of the four-role review of #21: only the registry had a
	// live-holder refusal test; --router took the exact same ProbeHeld path
	// (unlock.go's UnlockGlobal probes every scope the same way regardless
	// of which flag named it) but nothing exercised it.
	t.Run("refuses a live router lock holder, and clears nothing", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		state, err := layout.RouterState(env.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		routerScope := lock.Scope{Target: state.LockPath, Description: "shared router", Markers: procid.Markers{Project: "treerig-router"}}
		if err := os.MkdirAll(filepath.Dir(routerScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		lk := lock.New(routerScope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(ctx, "treerig router up"); err != nil {
			t.Fatal(err)
		}
		defer lk.Release()

		_, err = commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true})
		wantErr(t, err, `there is nothing to unlock`)
	})

	// Pins finding 1 of the four-role review of #21: --router used to build
	// the router's lock scope without ever calling router.SecureState, the
	// same guard RouterUp and newOverview both take before touching
	// the router's state directory, unlike ProbeHeld/Clear, which open the
	// lock file directly with nothing of their own to catch a symlink.
	t.Run("refuses a symlinked router state directory", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		home, _ := env.Getenv("HOME")
		real := filepath.Join(home, "elsewhere")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		state, err := layout.RouterState(env.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(state.StateDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, state.StateDir); err != nil {
			t.Fatal(err)
		}

		_, err = commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true})
		wantErr(t, err, `[Ss]ymlink`)
	})

	// Pins finding 1 of round 2 of the four-role review of #21: securing a
	// control home that is not there yet would create it (establishControlHome
	// adopts an empty directory, marker and all) — an unlock command has no
	// business doing that, so UnlockGlobal must find nothing to unlock
	// without ever touching the filesystem, the same as an absent lock.
	t.Run("creates nothing when the control home does not exist, and reports nothing to unlock", func(t *testing.T) {
		env, controlHome, _ := globalUnlockEnv(t)
		if _, err := os.Lstat(controlHome); !os.IsNotExist(err) {
			t.Fatalf("controlHome = %s, want it not to exist yet: %v", controlHome, err)
		}

		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 0 {
			t.Fatalf("cleared = %+v, want none", result.Cleared)
		}
		rec := env.Logger.(*logger.Recording)
		want := []string{"info: Nothing to unlock: no lock is held for the global worktree registry."}
		if !equalLines(rec.Lines, want) {
			t.Fatalf("lines = %v, want %v", rec.Lines, want)
		}
		if _, err := os.Lstat(controlHome); !os.IsNotExist(err) {
			t.Fatalf("controlHome = %s, want UnlockGlobal to have created nothing: %v", controlHome, err)
		}
	})

	// Router counterpart: router.SecureState's own secureDirectories calls
	// os.MkdirAll as readily as it secures a state directory already there,
	// so UnlockGlobal must skip it, not just refuse a symlink, when there is
	// nothing at the router's state directory yet.
	t.Run("creates nothing when the router state directory does not exist, and reports nothing to unlock", func(t *testing.T) {
		env, _, _ := globalUnlockEnv(t)
		state, err := layout.RouterState(env.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(state.StateDir); !os.IsNotExist(err) {
			t.Fatalf("state.StateDir = %s, want it not to exist yet: %v", state.StateDir, err)
		}

		result, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Router: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 0 {
			t.Fatalf("cleared = %+v, want none", result.Cleared)
		}
		rec := env.Logger.(*logger.Recording)
		want := []string{"info: Nothing to unlock: no lock is held for the shared router."}
		if !equalLines(rec.Lines, want) {
			t.Fatalf("lines = %v, want %v", rec.Lines, want)
		}
		if _, err := os.Lstat(state.StateDir); !os.IsNotExist(err) {
			t.Fatalf("state.StateDir = %s, want UnlockGlobal to have created nothing: %v", state.StateDir, err)
		}
	})

	// Registry counterpart of "refuses a symlinked router state directory"
	// above: once a control home is there at all (even just as a symlink),
	// UnlockGlobal must run the same secure check every other mutation
	// does before touching anything inside it.
	t.Run("refuses a symlinked control home", func(t *testing.T) {
		env, controlHome, _ := globalUnlockEnv(t)
		home, _ := env.Getenv("HOME")
		real := filepath.Join(home, "elsewhere")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, controlHome); err != nil {
			t.Fatal(err)
		}

		_, err := commands.UnlockGlobal(ctx, env, commands.UnlockOptions{Registry: true})
		wantErr(t, err, `[Ss]ymlink`)
	})
}

// equalLines reports whether got and want hold the same lines in the same order.
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

// hasPrefix reports whether s begins with prefix.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
