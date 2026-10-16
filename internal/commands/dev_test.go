package commands_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/run"
)

// logsProbe wraps a Runner and calls onLogs at the moment a `compose logs`
// is started, before the wrapped Runner answers it: the seam a test uses to
// look at what is held while the log follower runs.
type logsProbe struct {
	run.Runner
	onLogs    func()
	onLogsCtx func(ctx context.Context)
	// afterLogs, when set, replaces the wrapped Runner's answer to a `compose
	// logs`: the follower "ends" with whatever it returns.
	afterLogs func(ctx context.Context) error
}

// Run calls onLogs for a `compose logs` and then delegates every call.
func (p logsProbe) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if len(spec.Args) > 0 && spec.Args[0] == "compose" && strings.Contains(" "+strings.Join(spec.Args, " ")+" ", " logs ") {
		if p.onLogs != nil {
			p.onLogs()
		}
		if p.onLogsCtx != nil {
			p.onLogsCtx(ctx)
		}
		if p.afterLogs != nil {
			return run.Result{}, p.afterLogs(ctx)
		}
	}
	return p.Runner.Run(ctx, spec)
}

// pollRunner wraps a Runner to script and count the session's container
// polls (`compose ps --all --quiet`) and to hold the log follower until it
// has seen enough of them, which keeps a test off the clock.
type pollRunner struct {
	run.Runner
	mu      sync.Mutex
	outputs []string
	calls   int
	target  int
	reached chan struct{}
	// afterLogs, when set, decides how the follower ends once target polls
	// have been made.
	afterLogs func(ctx context.Context) error
	// followerExit is the follower's exit code once it ends.
	followerExit int
	// blockPS makes a container poll wait for its context to end, and take a
	// moment to return after that, counting those that started and those
	// that returned: a session that returns before its poll has is not
	// joining it.
	blockPS           bool
	started, returned int
}

// newPollRunner is a pollRunner over inner whose polls answer outputs in
// turn (the last repeating), and whose follower waits for target polls.
func newPollRunner(inner run.Runner, target int, outputs ...string) *pollRunner {
	return &pollRunner{Runner: inner, outputs: outputs, target: target, reached: make(chan struct{})}
}

// psInFlight is how many container polls have started and not yet returned.
func (p *pollRunner) psInFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started - p.returned
}

// psCalls is how many container polls have been made.
func (p *pollRunner) psCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Run answers a container poll from the script and a `compose logs` once the
// target number of polls has been made; every other call goes to the inner Runner.
func (p *pollRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	joined := " " + strings.Join(spec.Args, " ") + " "
	switch {
	case strings.Contains(joined, " ps --all --quiet "):
		p.mu.Lock()
		out := p.outputs[min(p.calls, len(p.outputs)-1)]
		p.calls++
		p.started++
		if p.calls == p.target {
			close(p.reached)
		}
		p.mu.Unlock()
		if p.blockPS {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // a poll slow to wind down
			p.mu.Lock()
			p.returned++
			p.mu.Unlock()
			return run.Result{}, context.Cause(ctx)
		}
		return run.Result{Stdout: out}, nil
	case strings.Contains(joined, " logs "):
		select {
		case <-p.reached:
		case <-ctx.Done():
			return run.Result{}, context.Cause(ctx)
		case <-time.After(20 * time.Second):
			return run.Result{}, errors.New("the container poll never ran enough")
		}
		if p.afterLogs != nil {
			return run.Result{}, p.afterLogs(ctx)
		}
		return run.Result{ExitCode: p.followerExit}, nil
	}
	return p.Runner.Run(ctx, spec)
}

// flockHeld reports whether another open file description holds the flock
// on path; a file that does not exist is free.
func flockHeld(t *testing.T, path string) bool {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil
}

func TestDevWithContext(t *testing.T) {
	t.Run("does up's work, announces the worktree, then follows the project's logs", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		err := commands.DevWithContext(context.Background(), tc.CommandContext, router, index)

		// The fake follower exits 0 at once, which is the session ending on its own.
		var failure *clierr.Error
		if !errors.As(err, &failure) || failure.ExitCode != 1 || !strings.Contains(failure.Message, "ended on its own (exit 0)") {
			t.Fatalf("DevWithContext = %v, want exit 1, the follower ending on its own", err)
		}
		if router.Ensures() != 1 {
			t.Fatalf("router ensured %d times, want once", router.Ensures())
		}
		if got := index.Reasons(); len(got) != 1 || got[0] != "up" {
			t.Fatalf("index refreshes = %v, want [up]", got)
		}
		lines, _ := tc.Logger.Snapshot()
		joined := strings.Join(lines, "\n")
		up := strings.Index(joined, "feature-login.acme.localhost is up.")
		following := strings.Index(joined, "Following the logs of feature-login.acme.localhost. Press Ctrl-C to end the session.")
		if up < 0 || following < up {
			t.Fatalf("log lines = %v, want the 'is up' line, then the 'Following' line", lines)
		}
		var ups, logs []string
		for _, call := range w.Docker.Calls() {
			switch {
			case strings.HasPrefix(call, "compose ") && strings.Contains(call, " up "):
				ups = append(ups, call)
			case strings.HasPrefix(call, "compose ") && strings.Contains(call, " logs "):
				logs = append(logs, call)
			}
		}
		if len(ups) != 1 || len(logs) != 1 || !strings.HasSuffix(logs[0], " logs --follow --tail 100") {
			t.Fatalf("compose up = %q, compose logs = %q, want one each, the latter `logs --follow --tail 100`", ups, logs)
		}
		if !strings.Contains(logs[0], "--project-name "+strings.Split(strings.Split(ups[0], "--project-name ")[1], " ")[0]) {
			t.Fatalf("logs %q follows another project than up %q started", logs[0], ups[0])
		}
	})

	t.Run("holds the development lock and not the operation lock while the logs are followed, and neither afterwards", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Lifecycle = lifecycle.NewLifecycle(lifecycle.New(), tc.Logger)
		var developmentHeld, operationHeld, probed bool
		paths := func() layout.Worktree {
			registration, err := tc.Registry.Current(context.Background())
			if err != nil || registration == nil {
				t.Fatalf("Current = %v, %v", registration, err)
			}
			return layout.WorktreeLayout(w.ControlHome, registration.RepositoryID, registration.WorktreeID)
		}
		tc.Runner = logsProbe{Runner: tc.Runner, onLogs: func() {
			probed = true
			developmentHeld = flockHeld(t, paths().DevelopmentLockPath)
			operationHeld = flockHeld(t, paths().LockPath)
		}}

		_ = commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		if !probed {
			t.Fatal("the log follower never ran")
		}
		if !developmentHeld || operationHeld {
			t.Fatalf("while following: development lock held = %v, operation lock held = %v; want true and false", developmentHeld, operationHeld)
		}
		if flockHeld(t, paths().DevelopmentLockPath) || flockHeld(t, paths().LockPath) {
			t.Fatal("a lock is still held after the session")
		}
	})

	t.Run("ends with 130 and says so when the follower is ended by Ctrl-C", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		w.Docker.FailCall(" logs ", "", 130, -1)

		err := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		var failure *clierr.Error
		if !errors.As(err, &failure) || failure.ExitCode != 130 || !failure.Notice ||
			failure.Message != `The development session of feature-login.acme.localhost ended (SIGINT). Its services are still running; "treerig down" stops them.` {
			t.Fatalf("DevWithContext = %v, want exit 130, a notice naming SIGINT", err)
		}
	})

	t.Run("exits with the follower's own code when it fails", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		w.Docker.FailCall(" logs ", "error from daemon in stream", 5, -1)

		err := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		var failure *clierr.Error
		if !errors.As(err, &failure) || failure.ExitCode != 5 || !strings.Contains(failure.Message, "ended on its own (exit 5)") {
			t.Fatalf("DevWithContext = %v, want exit 5, the follower ending on its own", err)
		}
	})

	t.Run("fails as up does, and follows nothing, when starting the services fails", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		w.Docker.FailUp("boom: cannot start", nil)
		_, upErr := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})

		devErr := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		if upErr == nil || devErr == nil || clierr.Code(devErr) != clierr.Code(upErr) || !strings.Contains(devErr.Error(), "boom: cannot start") {
			t.Fatalf("up = %v, dev = %v, want the same failure", upErr, devErr)
		}
		for _, call := range w.Docker.Calls() {
			if strings.Contains(call, " logs ") {
				t.Fatalf("logs were followed after a failed up: %q", call)
			}
		}
	})

	t.Run("refuses a control character in a service label, as up does, before touching anything", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "bad\x1b[31mlabel"}
		_, upErr := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})

		devErr := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		if upErr == nil || devErr == nil || devErr.Error() != upErr.Error() {
			t.Fatalf("up = %v, dev = %v, want the same refusal", upErr, devErr)
		}
		if len(w.Docker.Calls()) != 0 {
			t.Fatalf("docker was called: %v", w.Docker.Calls())
		}
	})
}

// indexProbe is an Index that records which of the worktree's locks are held
// at the moment it is asked to refresh, then optionally runs onRefresh.
type indexProbe struct {
	held      func() (operation, development bool)
	onRefresh func()
	calls     [][2]bool
}

// RefreshBestEffort records the held locks and runs onRefresh.
func (p *indexProbe) RefreshBestEffort(context.Context, string) {
	operation, development := p.held()
	p.calls = append(p.calls, [2]bool{operation, development})
	if p.onRefresh != nil {
		p.onRefresh()
	}
}

// worktreeLocks is the lock paths of the one worktree tc has registered.
func worktreeLocks(t *testing.T, w *commandstest.World, tc *commandstest.TestContext) layout.Worktree {
	t.Helper()
	registration, err := tc.Registry.Current(context.Background())
	if err != nil || registration == nil {
		t.Fatalf("Current = %v, %v", registration, err)
	}
	return layout.WorktreeLayout(w.ControlHome, registration.RepositoryID, registration.WorktreeID)
}

func TestDevSession(t *testing.T) {
	t.Run("refreshes the index with the development lock held and the operation lock not, after the URLs are printed", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		var order []string
		index := &indexProbe{held: func() (bool, bool) {
			paths := worktreeLocks(t, w, tc)
			lines, _ := tc.Logger.Snapshot()
			for _, line := range lines {
				if strings.Contains(line, "is up.") {
					order = append(order, "announced")
				}
			}
			return flockHeld(t, paths.LockPath), flockHeld(t, paths.DevelopmentLockPath)
		}}

		_ = commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), index)

		if len(index.calls) != 1 || index.calls[0] != [2]bool{false, true} {
			t.Fatalf("(operation, development) held at the refresh = %v, want one refresh with the development lock alone", index.calls)
		}
		if len(order) != 1 {
			t.Fatalf("the worktree was announced %d times before the refresh, want once", len(order))
		}
	})

	t.Run("returns the cancellation, and follows nothing, when the context ends between the two phases", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		ctx, cancel := context.WithCancelCause(context.Background())
		interrupted := &clierr.Cancelled{Signal: syscall.SIGTERM}
		index := &indexProbe{held: func() (bool, bool) { return false, false }, onRefresh: func() { cancel(interrupted) }}

		err := commands.DevWithContext(ctx, tc.CommandContext, commandstest.NewFakeRouter(80), index)

		if !errors.Is(err, interrupted) {
			t.Fatalf("DevWithContext = %v, want the cancellation, never nil", err)
		}
		for _, call := range w.Docker.Calls() {
			if strings.Contains(call, " logs ") {
				t.Fatalf("logs were followed on a cancelled context: %q", call)
			}
		}
	})

	t.Run("says the session ended, naming the signal, when a signal reaches treerig while it follows", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		ctx, cancel := context.WithCancelCause(context.Background())
		tc.Runner = logsProbe{Runner: tc.Runner, onLogs: func() { cancel(&clierr.Cancelled{Signal: syscall.SIGHUP}) }, afterLogs: func(ctx context.Context) error { return context.Cause(ctx) }}

		err := commands.DevWithContext(ctx, tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) || cancelled.Signal != syscall.SIGHUP {
			t.Fatalf("DevWithContext = %v, want the SIGHUP cancellation, for the process to die by", err)
		}
		lines, _ := tc.Logger.Snapshot()
		want := `The development session of feature-login.acme.localhost ended (SIGHUP). Its services are still running; "treerig down" stops them.`
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Fatalf("log lines = %v, want %q", lines, want)
		}
	})

	t.Run("does not put a deadline on the follower's context, whatever ran before it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare"), Seed: ns("./hooks/seed")}
		var sawLogs, deadline bool
		tc.Runner = logsProbe{Runner: tc.Runner, onLogsCtx: func(ctx context.Context) {
			sawLogs = true
			_, deadline = ctx.Deadline()
		}}

		_ = commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		if !sawLogs || deadline {
			t.Fatalf("log follower started = %v, its context has a deadline = %v; a hook's timeout must be the hook's own, never the shared context's", sawLogs, deadline)
		}
	})

	t.Run("says once that no container is left while it follows, and again each time the project empties anew", func(t *testing.T) {
		restore := commands.SetContainerPollForTest(time.Millisecond)
		defer restore()
		const notice = `No container of feature-login.acme.localhost is left, and "treerig worktree remove" refuses until the session ends; ` +
			`Ctrl-C ends the session, or run "treerig up" in another terminal to start them again.`
		for name, tc := range map[string]struct {
			outputs []string
			want    int
		}{
			"empty throughout":                         {[]string{"", "", ""}, 1},
			"empty, then containers, then empty again": {[]string{"", "abc\n", ""}, 2},
			"containers throughout":                    {[]string{"abc\n", "abc\n", "abc\n"}, 0},
		} {
			w := commandstest.New(t, "feature/login", acme)
			tc2 := w.Context("", acme, nil)
			givePorts(tc2)
			tc2.Runner = newPollRunner(tc2.Runner, len(tc.outputs), tc.outputs...)

			_ = commands.DevWithContext(context.Background(), tc2.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

			lines, _ := tc2.Logger.Snapshot()
			count := 0
			for _, line := range lines {
				if strings.Contains(line, notice) {
					count++
				}
			}
			if count != tc.want {
				t.Fatalf("%s: the no-container notice was printed %d times, want %d; lines = %v", name, count, tc.want, lines)
			}
		}
	})

	t.Run("drops 'its services are still running' from the closing notice when the last look found no container", func(t *testing.T) {
		restore := commands.SetContainerPollForTest(time.Millisecond)
		defer restore()
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Runner = newPollRunner(tc.Runner, 2, "", "")
		tc.Runner.(*pollRunner).followerExit = 130

		err := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})

		var failure *clierr.Error
		if !errors.As(err, &failure) || failure.Message != "The development session of feature-login.acme.localhost ended (SIGINT)." {
			t.Fatalf("DevWithContext = %v, want the notice without the services-running clause", err)
		}
	})

	t.Run("stops and joins the container poll when the follower ends, and when the context is cancelled", func(t *testing.T) {
		restore := commands.SetContainerPollForTest(time.Millisecond)
		defer restore()
		for name, cancelled := range map[string]bool{"the follower ends": false, "the context is cancelled": true} {
			w := commandstest.New(t, "feature/login", acme)
			tc := w.Context("", acme, nil)
			givePorts(tc)
			ctx, cancel := context.WithCancelCause(context.Background())
			polls := newPollRunner(tc.Runner, 1, "abc\n")
			polls.blockPS = true
			if cancelled {
				polls.afterLogs = func(ctx context.Context) error {
					cancel(&clierr.Cancelled{Signal: syscall.SIGTERM})
					return context.Cause(ctx)
				}
			}
			tc.Runner = polls

			_ = commands.DevWithContext(ctx, tc.CommandContext, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{})
			cancel(nil)

			// Joined before DevWithContext returned: the poll that was in
			// flight has returned, with no waiting here, and no more start.
			if polls.psCalls() == 0 {
				t.Fatalf("%s: the container poll never ran", name)
			}
			if inFlight := polls.psInFlight(); inFlight != 0 {
				t.Fatalf("%s: %d container polls were still running when the session returned", name, inFlight)
			}
			after := polls.psCalls()
			deadline := time.Now().Add(50 * time.Millisecond)
			for time.Now().Before(deadline) {
				if got := polls.psCalls(); got != after {
					t.Fatalf("%s: the container poll kept running after the session ended (%d calls, then %d)", name, after, got)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	})

	t.Run("worktree remove refuses while a session holds the development lock, and removes nothing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		if _, err := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		registration, err := tc.Registry.Current(context.Background())
		if err != nil || registration == nil {
			t.Fatalf("Current = %v, %v", registration, err)
		}
		_, development := lock.WorktreeScopes(w.ControlHome, registration.RepositoryID, registration.WorktreeID, registration.Slug,
			procid.Markers{})
		session := lock.New(development, lock.Options{Holder: "treerig dev", Logger: logger.Silent{}, Watchdog: -1})
		if _, err := session.Acquire(context.Background(), "treerig dev"); err != nil {
			t.Fatal(err)
		}
		defer session.Release()
		before := len(w.Docker.Calls())

		_, err = remove(t, tc, approved(commands.RemoveOptions{}))

		if err == nil || !strings.Contains(err.Error(), "feature-login development session") || !strings.Contains(err.Error(), "End it with Ctrl-C in the terminal running `treerig dev`") {
			t.Fatalf("remove = %v, want a refusal naming the development session and how to end it", err)
		}
		for _, call := range w.Docker.Calls()[before:] {
			if strings.Contains(call, " down") || strings.Contains(call, " rm ") {
				t.Fatalf("the refused removal touched Docker: %q", call)
			}
		}
		if found, _ := tc.Registry.Store(registration.RepositoryID).Find(registration.WorktreeID); found == nil {
			t.Fatal("the refused removal deleted the registration")
		}
	})
}
