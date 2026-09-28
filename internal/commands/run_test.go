package commands_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// runWorld is a registered worktree of a project with the given commands, a
// context for it, and a recorder for what `run` asks the runner to start.
type runWorld struct {
	*commandstest.World
	tc  *commandstest.TestContext
	ref worktree.Ref

	mu    sync.Mutex
	specs []run.Spec
	// during, when set, is called inside the command, while `run` still
	// holds whatever it holds; its result is the command's.
	during func(ctx context.Context, spec run.Spec) (run.Result, error)
}

// newRunWorld registers a worktree whose berth.toml declares commands.
func newRunWorld(t *testing.T, cmds config.Commands) *runWorld {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	rw := &runWorld{World: w}
	rw.ref = w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
	rw.tc = w.Context("", acme, nil)
	rw.tc.Loaded.Config.Commands = cmds
	rw.tc.Environ = []string{"PATH=/usr/bin"}
	fallback := w.Runner.Handler
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Output != run.Interactive {
			return fallback(ctx, spec)
		}
		rw.mu.Lock()
		rw.specs = append(rw.specs, spec)
		during := rw.during
		rw.mu.Unlock()
		if during != nil {
			return during(ctx, spec)
		}
		return run.Result{}, nil
	}
	return rw
}

// started is every interactive spec `run` has asked the runner for.
func (rw *runWorld) started() []run.Spec {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return append([]run.Spec(nil), rw.specs...)
}

// operationLock is a Lock over the worktree's operation lock that gives up
// after a moment instead of waiting out the real timeout, for probing
// whether it is held.
func (rw *runWorld) operationLock() *lock.Lock {
	scope, _ := lock.WorktreeScopes(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID, rw.ref.Slug, procid.Markers{})
	return lock.New(scope, lock.Options{Logger: logger.Silent{}, Timeout: 300 * time.Millisecond, Watchdog: -1})
}

// twoCommands is `plan` (a bare string, locked) and `apply` (a table).
func twoCommands() config.Commands {
	return config.Commands{
		{Name: "plan", Command: config.Command{Command: "./scripts/keycloak.sh plan", Lock: true, KillAfter: "10s"}},
		{Name: "apply", Command: config.Command{Command: "./scripts/keycloak.sh apply", Lock: true, KillAfter: "60s"}},
	}
}

func TestRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("runs the named command interactively with the arguments after -- appended, and BERTH_COMMAND", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		before := len(rw.Docker.Calls())

		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "apply", Args: []string{"-auto-approve", "a b"}})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		specs := rw.started()
		if len(specs) != 1 {
			t.Fatalf("started %d commands, want 1", len(specs))
		}
		spec := specs[0]
		if !strings.HasSuffix(spec.Command, "/scripts/keycloak.sh") || strings.Join(spec.Args, "|") != "apply|-auto-approve|a b" {
			t.Errorf("spec = %+v", spec)
		}
		if spec.Output != run.Interactive || spec.KillAfter != time.Minute || spec.InheritLocks {
			t.Errorf("spec = %+v, want interactive, a 60s kill-after and no inherited locks", spec)
		}
		if spec.Dir != rw.Root {
			t.Errorf("Dir = %q, want %q", spec.Dir, rw.Root)
		}
		want := map[string]string{
			"BERTH_COMMAND": "apply", "BERTH_WORKTREE_ID": rw.ref.WorktreeID, "BERTH_SLUG": "feature-login",
			"BERTH_HOST": layout.WorktreeHost("feature-login", acme),
		}
		for k, v := range want {
			if !containsEnv(spec.Env, k, v) {
				t.Errorf("want %s=%s in %v", k, v, spec.Env)
			}
		}
		if calls := rw.Docker.Calls()[before:]; len(calls) != 0 {
			t.Errorf("run touched Docker: %v", calls)
		}
	})

	t.Run("gives the command the routed URLs, with the router's port folded in", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		rw.tc.RouterPort = 8080
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
		env := rw.started()[0].Env
		if !containsEnv(env, "BERTH_URL", "http://feature-login.acme.localhost:8080") ||
			!containsEnv(env, "BERTH_SERVICE_URL_MAIL", "http://mail.feature-login.acme.localhost:8080") {
			t.Errorf("env = %v", env)
		}
	})

	t.Run("prints nothing while it runs, and nothing when it succeeds", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
		if lines, printed := rw.tc.Logger.Snapshot(); len(lines) != 0 || len(printed) != 0 {
			t.Fatalf("logged %v, printed %v", lines, printed)
		}
	})

	t.Run("passes the command's exit code through", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		rw.during = func(context.Context, run.Spec) (run.Result, error) { return run.Result{ExitCode: 3}, nil }
		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"})
		if err == nil || err.Error() != "The plan command failed (exit 3)." || clierr.Code(err) != 3 {
			t.Fatalf("err = %v (code %d)", err, clierr.Code(err))
		}
	})

	t.Run("passes a start failure's 127 through, saying command", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		rw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: spec.Command, Code: "ENOENT", Detail: "does not exist (ENOENT).", ExitCode: 127}
		}
		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"})
		if err == nil || !strings.HasPrefix(err.Error(), "The plan command could not be started: `") || clierr.Code(err) != 127 {
			t.Fatalf("err = %v (code %d)", err, clierr.Code(err))
		}
	})

	t.Run("without a name, lists the configured commands in file order", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{})
		want := "No command given: berth.toml defines plan, apply."
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if len(rw.started()) != 0 {
			t.Fatal("started a command")
		}
	})

	t.Run("an unknown name lists the configured commands", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "nope"})
		want := `No command "nope": berth.toml defines plan, apply.`
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("says so when berth.toml has no [commands]", func(t *testing.T) {
		rw := newRunWorld(t, nil)
		for _, tc := range []struct {
			opts commands.RunOptions
			want string
		}{
			{commands.RunOptions{}, "No command given: berth.toml has no [commands]."},
			{commands.RunOptions{Name: "nope"}, `No command "nope": berth.toml has no [commands].`},
		} {
			err := commands.RunWithContext(ctx, rw.tc.CommandContext, tc.opts)
			if err == nil || err.Error() != tc.want || clierr.Code(err) != 1 {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		}
	})

	t.Run("refuses a worktree that is not registered, and starts nothing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Commands = twoCommands()
		var started int
		git := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Output == run.Interactive {
				started++
			}
			return git(ctx, spec)
		}

		err := commands.RunWithContext(ctx, tc.CommandContext, commands.RunOptions{Name: "plan"})
		want := "This worktree is not registered. Run 'berth up' first."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if started != 0 {
			t.Fatalf("ran %d commands", started)
		}
	})

	t.Run("holds the worktree's operation lock while the command runs, under the run's name and without its arguments", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		var heldErr error
		rw.during = func(context.Context, run.Spec) (run.Result, error) {
			_, heldErr = rw.operationLock().Acquire(ctx, "probe")
			return run.Result{}, nil
		}
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "apply", Args: []string{"-var", "token=hunter2"}}); err != nil {
			t.Fatal(err)
		}
		if heldErr == nil {
			t.Fatal("the operation lock was free while the command ran")
		}
		if !strings.Contains(heldErr.Error(), `"berth run apply"`) || strings.Contains(heldErr.Error(), "hunter2") {
			t.Errorf("a waiter is told %q, want it to name `berth run apply` and no argument", heldErr)
		}

		lk := rw.operationLock()
		if _, err := lk.Acquire(ctx, "probe"); err != nil {
			t.Fatalf("the lock is still held after run returned: %v", err)
		}
		_ = lk.Release()
	})

	t.Run("releases the lock when the command is interrupted", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		interrupted, interrupt := context.WithCancelCause(ctx)
		rw.during = func(ctx context.Context, _ run.Spec) (run.Result, error) {
			interrupt(&clierr.Cancelled{Signal: 2})
			<-ctx.Done()
			return run.Result{}, context.Cause(ctx)
		}
		err := commands.RunWithContext(interrupted, rw.tc.CommandContext, commands.RunOptions{Name: "apply"})
		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		lk := rw.operationLock()
		if _, err := lk.Acquire(ctx, "probe"); err != nil {
			t.Fatalf("the lock is still held after an interrupt: %v", err)
		}
		_ = lk.Release()
	})

	t.Run("lock = false takes no lock, and still runs the command", func(t *testing.T) {
		cmds := config.Commands{{Name: "plan", Command: config.Command{Command: "./plan.sh", Lock: false, KillAfter: "10s"}}}
		rw := newRunWorld(t, cmds)
		var acquireErr error
		rw.during = func(context.Context, run.Spec) (run.Result, error) {
			lk := rw.operationLock()
			_, acquireErr = lk.Acquire(ctx, "probe")
			if acquireErr == nil {
				_ = lk.Release()
			}
			return run.Result{}, nil
		}
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
		if acquireErr != nil {
			t.Fatalf("a lock = false command held the lock: %v", acquireErr)
		}
		if len(rw.started()) != 1 {
			t.Fatal("did not run")
		}
	})
}

// A locked command inside its own worktree's environment would wait on the
// lock its caller holds; it refuses at once, before taking anything (#102).
func TestRunNested(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := layout.WorktreeHost("feature-login", acme)

	t.Run("a locked command refuses inside its own worktree's environment, before running anything", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		inside(rw.tc, rw.ref.WorktreeID)
		err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"})
		requireNestedRefusal(t, err, host)
		if len(rw.started()) != 0 {
			t.Fatal("ran the command")
		}
	})

	t.Run("a lock = false command has no lock to wait on, so it runs", func(t *testing.T) {
		cmds := config.Commands{{Name: "plan", Command: config.Command{Command: "./plan.sh", Lock: false, KillAfter: "10s"}}}
		rw := newRunWorld(t, cmds)
		inside(rw.tc, rw.ref.WorktreeID)
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a locked command runs from another worktree's environment", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		inside(rw.tc, "00000000-0000-4000-8000-000000000000")
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
	})
}

// containsEnv reports whether env holds exactly key=value.
func containsEnv(env []string, key, value string) bool {
	for _, entry := range env {
		if entry == key+"="+value {
			return true
		}
	}
	return false
}
