//go:build integration

package commands_test

// Port of the `up` describe block in
// tests/integration/commands.integration.test.ts: the command layer end to
// end — a real Git repository, real hook executables, real locks, a real
// process runner — with commandstest.Docker answering every `docker` call in
// place of the TypeScript suite's MemoryRuntime. Plus two interrupts the
// TypeScript suite never had: one landing in `compose up`'s own child, one
// landing in a hook, each a real process tree that must be stopped with its
// whole group before the worktree lock is released.
//
// An interrupt here is what lifecycle.Lifecycle turns a SIGINT into: the
// root context cancelled with cause *clierr.Cancelled. Dying by the signal
// afterwards is cli.Main's job; internal/cli's TestUpThroughTheBinary sends
// `up` real signals in each of its phases and proves it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/osrun"
	"golang.org/x/sys/unix"
)

// upConfig is the TypeScript suite's CONFIG, plus a port on each service:
// commandstest.Docker's `compose config` declares no ports, and a routed
// service with neither is refused before anything starts.
const upConfig = `
[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
port = 8080
[service.index]
label = "API"
[[service]]
name = "mail"
subdomain = "mail"
path = "/inbox"
port = 8080
[hooks]
prepare = "./hooks/record"
seed = "./hooks/record"
verify = "./hooks/record"
`

// fakeDockerRunner is a real process runner for everything but `docker`,
// which commandstest.Docker answers — except `compose up`, which composeUp
// answers when set, so a test can put a real process tree there.
type fakeDockerRunner struct {
	*osrun.Runner
	docker    *commandstest.Docker
	composeUp func(ctx context.Context, spec run.Spec) (run.Result, error)
}

// Run passes every non-docker command to the real runner, routes `docker`
// through the fake daemon, and gives `compose up` a real running container
// per service — or, when composeUp is set, hands it a real process tree
// instead.
func (r *fakeDockerRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if spec.Command != "docker" {
		return r.Runner.Run(ctx, spec)
	}
	if len(spec.Args) > 0 && spec.Args[0] == "compose" && slices.Contains(spec.Args, "up") {
		if r.composeUp != nil {
			return r.composeUp(ctx, spec)
		}
		// What `compose up` leaves behind: a running container for each
		// service, under the project `compose ps` is asked about.
		project := spec.Args[slices.Index(spec.Args, "--project-name")+1]
		for _, service := range r.docker.Services {
			r.docker.Add("container", map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service})
		}
		return run.Finish(spec, run.Result{})
	}
	return r.docker.Handle(ctx, spec)
}

type upWorld struct {
	base, root, controlHome, log, failFile string
	docker                                 *commandstest.Docker
}

// newUpWorld is a fresh checkout with a config, a Compose file, and a hook
// executable that records every stage it ran, fails prepare on demand, and
// can be made to hang so a test can interrupt it mid-hook.
func newUpWorld(t *testing.T) *upWorld {
	t.Helper()
	base := registrytest.TempDir(t)
	w := &upWorld{
		base: base, root: filepath.Join(base, "repo"), controlHome: filepath.Join(base, "control"),
		log: filepath.Join(base, "hooks.log"), failFile: filepath.Join(base, "fail-prepare"),
		docker: commandstest.NewDocker("api", "mail"),
	}
	if err := os.MkdirAll(filepath.Join(w.root, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.root, "init", "--initial-branch=feature/login", ".")
	gitIn(t, w.root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")
	writeFile(t, filepath.Join(w.root, "berth.toml"), upConfig, 0o644)
	writeFile(t, filepath.Join(w.root, "compose.yaml"), "services: {}\n", 0o644)
	// Records what it saw; fails prepare with exit 7 while the fail file
	// exists; proves WORKTREE_HOME is writable. A stage named in
	// $BERTH_TEST_HANG_STAGE writes its pid and waits, in a grandchild, so
	// only a group kill stops it.
	writeFile(t, w.hook(), strings.Join([]string{
		"#!/bin/sh",
		fmt.Sprintf(`echo "$BERTH_STAGE $BERTH_SLUG $BERTH_HOST $BERTH_PROJECT $(pwd)" >> %q`, w.log),
		`touch "$BERTH_WORKTREE_HOME/touched-by-$BERTH_STAGE" || exit 9`,
		fmt.Sprintf(`if [ "$BERTH_STAGE" = prepare ] && [ -e %q ]; then exit 7; fi`, w.failFile),
		fmt.Sprintf(`if [ -e %q ]; then sh -c 'echo $$ > "$0"; exec sleep 60' %q & wait; fi`,
			filepath.Join(base, "hang-$BERTH_STAGE"), filepath.Join(base, "hung.pid")),
		"exit 0",
		"",
	}, "\n"), 0o755)
	return w
}

// hook returns the path of the hook executable every stage runs.
func (w *upWorld) hook() string { return filepath.Join(w.root, "hooks", "record") }

// writeFile writes contents to path with the given mode, failing the test
// on error.
func writeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

// context is a fresh CommandContext over the world, as one `up` invocation
// builds it, with a fake router and index: routing is not under test here.
func (w *upWorld) context(t *testing.T, runner *fakeDockerRunner) (*commands.CommandContext, *logger.Recording) {
	t.Helper()
	if runner == nil {
		runner = w.runner()
	}
	log := &logger.Recording{}
	getenv := func(key string) (string, bool) {
		switch key {
		case "BERTH_HOME":
			return w.controlHome, true
		case "XDG_STATE_HOME":
			return filepath.Join(w.base, "state"), true
		}
		return "", false
	}
	registrytest.RequireIsolatedRouterState(t, getenv)
	cctx, err := commands.Context(&commands.Env{
		CommandName: "berth", Invocation: "berth up", Logger: log, Getenv: getenv,
		Environ: os.Environ(), Cwd: w.root, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cctx, log
}

// runner is a fresh fakeDockerRunner over a real process runner and the
// world's fake daemon.
func (w *upWorld) runner() *fakeDockerRunner {
	return &fakeDockerRunner{Runner: osrun.New(), docker: w.docker}
}

// up runs `up` against a background context, with a fake router and index.
func (w *upWorld) up(t *testing.T, cctx *commands.CommandContext, services ...string) (*commands.UpResult, error) {
	t.Helper()
	return w.upWithContext(context.Background(), cctx, services...)
}

// upWithContext runs `up` against ctx, so a test can cancel it mid-flight,
// with a fake router and index.
func (w *upWorld) upWithContext(ctx context.Context, cctx *commands.CommandContext, services ...string) (*commands.UpResult, error) {
	return commands.UpWithContext(ctx, cctx, commandstest.NewFakeRouter(80, "berth-router"), &commandstest.FakeIndex{},
		commands.UpOptions{Services: services})
}

// hookLog returns the hook's recorded lines, or nil if it never ran.
func (w *upWorld) hookLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(w.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// stages returns just the stage name from each recorded hook line, in the
// order the hooks ran.
func (w *upWorld) stages(t *testing.T) []string {
	t.Helper()
	var stages []string
	for _, line := range w.hookLog(t) {
		stages = append(stages, strings.Fields(line)[0])
	}
	return stages
}

// initialization is what the registry says of this checkout now, read afresh.
func (w *upWorld) initialization(t *testing.T) (registry.WorktreeInitialization, worktreeIDs) {
	t.Helper()
	cctx, _ := w.context(t, nil)
	registration, err := cctx.Registry.Current(context.Background())
	if err != nil || registration == nil {
		t.Fatalf("registration = %v, %v", registration, err)
	}
	state, err := cctx.Registry.Store(registration.RepositoryID).Initialization(registration.WorktreeID, registration.Slug)
	if err != nil {
		t.Fatal(err)
	}
	return state, worktreeIDs{registration.RepositoryID, registration.WorktreeID}
}

type worktreeIDs struct{ repositoryID, worktreeID string }

// requireReleased checks the worktree lock is free now, and that its holder
// record says it was released rather than abandoned.
func (w *upWorld) requireReleased(t *testing.T, ids worktreeIDs) {
	t.Helper()
	paths := layout.WorktreeLayout(w.controlHome, ids.repositoryID, ids.worktreeID)
	file, err := os.OpenFile(paths.LockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("the worktree lock is still held: %v", err)
	}
	holder, err := os.ReadFile(paths.LockPath + ".holder")
	if err != nil || !strings.Contains(string(holder), `"releasedAt"`) {
		t.Fatalf("holder record = %s, %v", holder, err)
	}
}

// interruptWhen cancels a context the way a SIGINT does, once path appears,
// and returns the pid written there.
func interruptWhen(t *testing.T, path string) (context.Context, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	pid := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(path); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n > 0 {
					pid <- n
					cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		pid <- 0
		cancel(errors.New("the process to interrupt never started"))
	}()
	return ctx, func() int {
		n := <-pid
		if n > 0 {
			t.Cleanup(func() { _ = unix.Kill(n, syscall.SIGKILL) })
		}
		return n
	}
}

// requireCancelled fails the test unless err is a *clierr.Cancelled from
// SIGINT, the shape an interrupted `up` must fail with.
func requireCancelled(t *testing.T, err error) {
	t.Helper()
	var cancelled *clierr.Cancelled
	if !errors.As(err, &cancelled) || cancelled.Signal != syscall.SIGINT {
		t.Fatalf("err = %v, want cancellation by SIGINT", err)
	}
}

func TestUpEndToEnd(t *testing.T) {
	t.Run("registers the worktree, starts services, runs prepare then seed the first time, and prints URLs", func(t *testing.T) {
		w := newUpWorld(t)
		cctx, log := w.context(t, nil)

		result, err := w.up(t, cctx, "api", "mail")
		if err != nil {
			t.Fatal(err)
		}

		if result.Registration.Slug != "feature-login" {
			t.Fatalf("slug = %q", result.Registration.Slug)
		}
		if !slices.Equal(stageNames(result), []string{"prepare", "seed"}) {
			t.Fatalf("hooks = %v", result.Hooks)
		}
		var urls []string
		for _, u := range result.URLs {
			urls = append(urls, u.URL)
		}
		if want := []string{"http://feature-login.acme.localhost", "http://mail.feature-login.acme.localhost/inbox"}; !slices.Equal(urls, want) {
			t.Fatalf("urls = %v, want %v", urls, want)
		}
		if want := []string{"api   http://feature-login.acme.localhost", "mail  http://mail.feature-login.acme.localhost/inbox"}; !slices.Equal(log.Printed, want) {
			t.Fatalf("printed = %q, want %q", log.Printed, want)
		}
		// Hooks saw the contract: stage, slug, host, project, and the project root as cwd.
		if want := []string{
			"prepare feature-login feature-login.acme.localhost acme " + w.root,
			"seed feature-login feature-login.acme.localhost acme " + w.root,
		}; !slices.Equal(w.hookLog(t), want) {
			t.Fatalf("hook log = %q, want %q", w.hookLog(t), want)
		}
		if state, _ := w.initialization(t); state != registry.Complete {
			t.Fatalf("initialization = %v, want complete", state)
		}
		statuses, err := commands.StatusWithContext(context.Background(), cctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(statuses); got != "[{api running } {mail running }]" {
			t.Fatalf("status = %s", got)
		}
	})

	t.Run("reports a hook that cannot be started, not a failed exit", func(t *testing.T) {
		w := newUpWorld(t)
		if err := os.Chmod(w.hook(), 0o644); err != nil {
			t.Fatal(err)
		}
		cctx, _ := w.context(t, nil)

		_, err := w.up(t, cctx)

		want := fmt.Sprintf("The prepare hook could not be started: `%s` is not executable (EACCES).", w.hook())
		if err == nil || err.Error() != want || clierr.Code(err) != 126 {
			t.Fatalf("err = %v (exit %d), want %q (exit 126)", err, clierr.Code(err), want)
		}
		if log := w.hookLog(t); len(log) != 0 {
			t.Fatalf("hook log = %q, want empty", log)
		}
	})

	t.Run("runs only prepare on later ups", func(t *testing.T) {
		w := newUpWorld(t)
		first, _ := w.context(t, nil)
		if _, err := w.up(t, first); err != nil {
			t.Fatal(err)
		}
		again, _ := w.context(t, nil)
		second, err := w.up(t, again)
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(stageNames(second), []string{"prepare"}) {
			t.Fatalf("hooks = %v", second.Hooks)
		}
		if want := []string{"prepare", "seed", "prepare"}; !slices.Equal(w.stages(t), want) {
			t.Fatalf("stages = %v, want %v", w.stages(t), want)
		}
	})

	t.Run("never runs seed when prepare fails, keeps the worktree pending, preserves the exit code, and releases the lock", func(t *testing.T) {
		w := newUpWorld(t)
		writeFile(t, w.failFile, "", 0o644)
		cctx, _ := w.context(t, nil)

		_, err := w.up(t, cctx)
		if err == nil || clierr.Code(err) != 7 || !strings.Contains(err.Error(), "prepare hook failed") {
			t.Fatalf("err = %v (exit %d), want the prepare hook's failure with exit 7", err, clierr.Code(err))
		}
		if want := []string{"prepare"}; !slices.Equal(w.stages(t), want) {
			t.Fatalf("stages = %v, want %v", w.stages(t), want)
		}
		state, ids := w.initialization(t)
		if state != registry.Pending {
			t.Fatalf("initialization = %v, want pending", state)
		}
		w.requireReleased(t, ids)

		// The lock was released on the failure path; fixing the hook and retrying seeds as a first run.
		if err := os.Remove(w.failFile); err != nil {
			t.Fatal(err)
		}
		retry, _ := w.context(t, nil)
		result, err := w.up(t, retry)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(stageNames(result), []string{"prepare", "seed"}) {
			t.Fatalf("hooks = %v", result.Hooks)
		}
	})

	t.Run("defers an interrupt until the lock is released, then re-raises it", func(t *testing.T) {
		w := newUpWorld(t)
		pidFile := filepath.Join(w.base, "compose-up.pid")
		runner := w.runner()
		// A `compose up` that is still starting when the interrupt lands: a
		// real child, whose own child only a group kill stops.
		runner.composeUp = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			spec.Command = "sh"
			spec.Args = []string{"-c", `sh -c 'echo $$ > "$0"; exec sleep 60' "$1" & wait`, "sh", pidFile}
			return runner.Runner.Run(ctx, spec)
		}
		cctx, _ := w.context(t, runner)
		ctx, grandchild := interruptWhen(t, pidFile)

		_, err := w.upWithContext(ctx, cctx, "api")

		requireCancelled(t, err)
		if pid := grandchild(); pid == 0 || unix.Kill(pid, 0) == nil {
			t.Fatalf("compose up's process group survived the interrupt: %d", pid)
		}
		if log := w.hookLog(t); len(log) != 0 { // cancelled before any hook could run
			t.Fatalf("hook log = %q, want empty", log)
		}
		state, ids := w.initialization(t)
		if state != registry.Pending {
			t.Fatalf("initialization = %v, want pending", state)
		}
		w.requireReleased(t, ids)
	})

	t.Run("stops a hook an interrupt lands in, with its process group, never runs seed, and releases the lock", func(t *testing.T) {
		w := newUpWorld(t)
		writeFile(t, filepath.Join(w.base, "hang-prepare"), "", 0o644)
		cctx, _ := w.context(t, nil)
		ctx, hung := interruptWhen(t, filepath.Join(w.base, "hung.pid"))

		_, err := w.upWithContext(ctx, cctx)

		requireCancelled(t, err)
		if pid := hung(); pid == 0 || unix.Kill(pid, 0) == nil {
			t.Fatalf("the prepare hook's process group survived the interrupt: %d", pid)
		}
		if want := []string{"prepare"}; !slices.Equal(w.stages(t), want) {
			t.Fatalf("stages = %v, want %v", w.stages(t), want)
		}
		state, ids := w.initialization(t)
		if state != registry.Pending {
			t.Fatalf("initialization = %v, want pending", state)
		}
		w.requireReleased(t, ids)
	})
}

// stageNames extracts each hook stage's name from result.Hooks, in the
// order they ran.
func stageNames(result *commands.UpResult) []string {
	var names []string
	for _, stage := range result.Hooks {
		names = append(names, string(stage))
	}
	return names
}
