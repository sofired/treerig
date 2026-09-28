package hooks_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/fakerun"
	"github.com/sofired/berth/internal/worktree"
)

// testRef and testSource build a fixed hook environment: PROJECT acme,
// HOST feature-login.acme.localhost, ROOT /work/acme, and so on.
var (
	testRef = worktree.Ref{RepositoryID: "repo-a", WorktreeID: "wt-1", Slug: "feature-login"}
	testSrc = worktree.EnvironmentSource{
		ProjectName: "acme", Domain: "acme.localhost", ControlHome: "/control",
		ConfigFile: "/work/acme/berth.toml", ProjectRoot: "/work/acme",
	}
)

// newContext builds a HookContext over the fixed testRef/testSrc with the
// given runner and hook configuration, discarding log output.
func newContext(runner run.Runner, hooksCfg config.Hooks) hooks.HookContext {
	return hooks.HookContext{Source: testSrc, Ref: testRef, Hooks: hooksCfg, Logger: logger.Silent{}, Runner: runner}
}

// str returns a pointer to s, for populating *string config fields inline.
func str(s string) *string { return &s }

// recordingRunner answers every command with a fixed exit code (0 by
// default), recording every call.
func recordingRunner(exitCodes map[string]int) *fakerun.Runner {
	return &fakerun.Runner{Handler: func(_ context.Context, spec run.Spec) (run.Result, error) {
		return run.Result{ExitCode: exitCodes[spec.Command]}, nil
	}}
}

// brokenRunner is a runner whose every command never runs (a spawn failure)
// or is ended by a signal or a timeout.
func brokenRunner(outcome string) *fakerun.Runner {
	return &fakerun.Runner{Handler: func(_ context.Context, spec run.Spec) (run.Result, error) {
		switch outcome {
		case "ENOENT":
			return run.Result{}, &run.StartError{
				Command: spec.Command, Code: "ENOENT",
				Detail:   "does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).",
				ExitCode: 127,
			}
		case "EACCES":
			return run.Result{}, &run.StartError{Command: spec.Command, Code: "EACCES", Detail: "is not executable (EACCES).", ExitCode: 126}
		case "SIGKILL":
			return run.Result{ExitCode: 137, Signal: syscall.SIGKILL}, nil
		case "TIMEOUT":
			return run.Result{TimedOut: true}, nil
		default:
			return run.Result{}, nil
		}
	}}
}

func TestRunHook(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves a relative command against the project root and passes the stage environment", func(t *testing.T) {
		runner := recordingRunner(nil)
		ran, err := hooks.RunHook(ctx, config.Prepare, newContext(runner, config.Hooks{Prepare: str("./scripts/migrate.sh --fast")}), hooks.RunOptions{})
		if err != nil || !ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		calls := runner.Calls()
		if len(calls) != 1 {
			t.Fatalf("expected one call, got %d", len(calls))
		}
		call := calls[0]
		if call.Command != "/work/acme/scripts/migrate.sh" {
			t.Fatalf("got command %q", call.Command)
		}
		if len(call.Args) != 1 || call.Args[0] != "--fast" {
			t.Fatalf("got args %v", call.Args)
		}
		if call.Dir != "/work/acme" {
			t.Fatalf("got dir %q", call.Dir)
		}
		want := map[string]string{
			"BERTH_STAGE": "prepare", "BERTH_SLUG": "feature-login",
			"BERTH_HOST":          "feature-login.acme.localhost",
			"BERTH_WORKTREE_HOME": "/control/repositories/repo-a/instances/wt-1",
			"BERTH_ROOT":          "/work/acme",
		}
		for k, v := range want {
			if !containsEnv(call.Env, k, v) {
				t.Fatalf("expected %s=%s in env %v", k, v, call.Env)
			}
		}
	})

	t.Run("passes BERTH_URL for the root route and BERTH_SERVICE_URL_<NAME> for every routed service (#28)", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./run.sh")})
		hctx.Routes = worktree.Routes([]config.Service{
			{Name: "api"},
			{Name: "mail-relay", Subdomain: "mail"},
		}, hctx.Ref.Slug, hctx.Source.Domain, 8443)
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"BERTH_URL":                    "http://feature-login.acme.localhost:8443",
			"BERTH_SERVICE_URL_API":        "http://feature-login.acme.localhost:8443",
			"BERTH_SERVICE_URL_MAIL_RELAY": "http://mail.feature-login.acme.localhost:8443",
		}
		env := runner.Calls()[0].Env
		for k, v := range want {
			if !containsEnv(env, k, v) {
				t.Fatalf("expected %s=%s in env %v", k, v, env)
			}
		}
	})

	t.Run("passes neither BERTH_URL nor any BERTH_SERVICE_URL_ variable when no routes were given", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./run.sh")})
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, entry := range runner.Calls()[0].Env {
			name, _, _ := strings.Cut(entry, "=")
			if name == "BERTH_URL" || strings.HasPrefix(name, "BERTH_SERVICE_URL_") {
				t.Fatalf("unexpected %s in env %v", name, runner.Calls()[0].Env)
			}
		}
	})

	t.Run("passes the user's control-home override through untouched, so a hook can call the tool", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./run.sh")})
		hctx.Environ = []string{"BERTH_HOME=/custom/control-home"}
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		if !containsEnv(runner.Calls()[0].Env, "BERTH_HOME", "/custom/control-home") {
			t.Fatalf("expected BERTH_HOME to pass through, got %v", runner.Calls()[0].Env)
		}
	})

	t.Run("leaves bare command names to PATH resolution and appends the bundle argument", func(t *testing.T) {
		runner := recordingRunner(nil)
		arg := "/bundles/one"
		_, err := hooks.RunHook(ctx, config.Backup, newContext(runner, config.Hooks{Backup: str("pg_dump")}), hooks.RunOptions{Argument: &arg})
		if err != nil {
			t.Fatal(err)
		}
		call := runner.Calls()[0]
		if call.Command != "pg_dump" {
			t.Fatalf("got command %q", call.Command)
		}
		if len(call.Args) != 1 || call.Args[0] != "/bundles/one" {
			t.Fatalf("got args %v", call.Args)
		}
	})

	t.Run("returns false without spawning when the stage is not configured", func(t *testing.T) {
		runner := recordingRunner(nil)
		ran, err := hooks.RunHook(ctx, config.Seed, newContext(runner, config.Hooks{}), hooks.RunOptions{})
		if err != nil || ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		if len(runner.Calls()) != 0 {
			t.Fatalf("expected no calls, got %v", runner.Calls())
		}
	})

	t.Run("attributes a non-zero exit to the stage and preserves the exit code", func(t *testing.T) {
		runner := recordingRunner(map[string]int{"/work/acme/seed.py": 3})
		_, err := hooks.RunHook(ctx, config.Seed, newContext(runner, config.Hooks{Seed: str("./seed.py")}), hooks.RunOptions{})
		requireExit(t, err, 3, "The seed hook failed (exit 3)")
	})

	t.Run("a hook body of a single quoted empty string is refused as empty, not as a missing command", func(t *testing.T) {
		// splitCommand(`""`) answers [""] (one empty-string token), and an
		// empty first token is refused the same way a whitespace-only
		// command is — never spawned,
		// never reported as "not on PATH".
		runner := recordingRunner(nil)
		_, err := hooks.RunHook(ctx, config.Prepare, newContext(runner, config.Hooks{Prepare: str(`""`)}), hooks.RunOptions{})
		requireExit(t, err, 1, `Hook "prepare" is empty.`)
		if len(runner.Calls()) != 0 {
			t.Fatalf("expected no calls, got %v", runner.Calls())
		}
	})
}

func TestRunHookWhenTheHookNeverRuns(t *testing.T) {
	ctx := context.Background()

	t.Run("names the resolved path of a hook that does not exist, with the shell's exit code", func(t *testing.T) {
		_, err := hooks.RunHook(ctx, config.Prepare, newContext(brokenRunner("ENOENT"), config.Hooks{Prepare: str("./scripts/missing.sh")}), hooks.RunOptions{})
		requireMessage(t, err, 127,
			"The prepare hook could not be started: `/work/acme/scripts/missing.sh` does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).")
	})

	t.Run("says when a hook is not executable, rather than reporting a failed exit", func(t *testing.T) {
		_, err := hooks.RunHook(ctx, config.Seed, newContext(brokenRunner("EACCES"), config.Hooks{Seed: str("./scripts/seed")}), hooks.RunOptions{})
		requireMessage(t, err, 126, "The seed hook could not be started: `/work/acme/scripts/seed` is not executable (EACCES).")
	})

	t.Run("reports the signal that ended a hook", func(t *testing.T) {
		_, err := hooks.RunHook(ctx, config.Prepare, newContext(brokenRunner("SIGKILL"), config.Hooks{Prepare: str("make prepare")}), hooks.RunOptions{})
		requireMessage(t, err, 137, "The prepare hook was terminated by SIGKILL.")
	})

	t.Run("reports a hook that drowned its own output", func(t *testing.T) {
		runner := &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) {
			return run.Result{ExitCode: 1, OutputOverflow: true}, nil
		}}
		_, err := hooks.RunHook(ctx, config.Prepare, newContext(runner, config.Hooks{Prepare: str("make noisy")}), hooks.RunOptions{})
		requireMessage(t, err, 1, "The prepare hook produced more output than can be captured and was stopped.")
	})

	t.Run("reports a timeout as a timeout, whatever the hook managed to exit with", func(t *testing.T) {
		_, err := hooks.RunHook(ctx, config.Seed, newContext(brokenRunner("TIMEOUT"), config.Hooks{Seed: str("make seed")}), hooks.RunOptions{Timeout: 50 * time.Millisecond})
		requireMessage(t, err, 124, "The seed hook timed out after 50 ms and was terminated.")
	})
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("runs prepare, seed, and verify in order", func(t *testing.T) {
		runner := recordingRunner(nil)
		ran, err := hooks.RunLifecycle(ctx, newContext(runner, config.Hooks{Prepare: str("a"), Seed: str("b"), Verify: str("c")}), hooks.LifecycleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []config.HookStage{config.Prepare, config.Seed, config.Verify}; !slices.Equal(ran, want) {
			t.Fatalf("got %v, want %v", ran, want)
		}
		var commands []string
		for _, call := range runner.Calls() {
			commands = append(commands, call.Command)
		}
		if want := []string{"a", "b", "c"}; !slices.Equal(commands, want) {
			t.Fatalf("got %v, want %v", commands, want)
		}
	})

	t.Run("never runs seed when prepare fails", func(t *testing.T) {
		runner := recordingRunner(map[string]int{"a": 1})
		_, err := hooks.RunLifecycle(ctx, newContext(runner, config.Hooks{Prepare: str("a"), Seed: str("b")}), hooks.LifecycleOptions{})
		if err == nil || !strings.Contains(err.Error(), "The prepare hook failed") {
			t.Fatalf("expected a prepare failure, got %v", err)
		}
		var commands []string
		for _, call := range runner.Calls() {
			commands = append(commands, call.Command)
		}
		if want := []string{"a"}; !slices.Equal(commands, want) {
			t.Fatalf("expected seed never to run, got %v", commands)
		}
	})

	t.Run("skips unconfigured stages without treating them as failures", func(t *testing.T) {
		runner := recordingRunner(nil)
		ran, err := hooks.RunLifecycle(ctx, newContext(runner, config.Hooks{Verify: str("c")}), hooks.LifecycleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []config.HookStage{config.Verify}; !slices.Equal(ran, want) {
			t.Fatalf("got %v, want %v", ran, want)
		}
	})
}

// splitCommand is unexported; its behaviour is exercised indirectly through
// RunHook's argument parsing above (relative-path resolution, quoted
// segments via the bundle-argument test) and directly by TestSplitCommand's
// package-internal counterpart — see split_test.go.

// containsEnv reports whether env has an entry exactly matching key=value.
func containsEnv(env []string, key, value string) bool {
	for _, entry := range env {
		if entry == key+"="+value {
			return true
		}
	}
	return false
}

// requireExit fails the test unless err is non-nil, its message contains
// messageContains, and its exit code equals code.
func requireExit(t *testing.T, err error, code int, messageContains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q", messageContains)
	}
	if !strings.Contains(err.Error(), messageContains) {
		t.Fatalf("got %q, want it to contain %q", err.Error(), messageContains)
	}
	if got := clierr.Code(err); got != code {
		t.Fatalf("got exit code %d, want %d", got, code)
	}
}

// requireMessage fails the test unless err is non-nil, its message equals
// message exactly, and its exit code equals code.
func requireMessage(t *testing.T, err error, code int, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, want %q", message)
	}
	if err.Error() != message {
		t.Fatalf("got %q, want %q", err.Error(), message)
	}
	if got := clierr.Code(err); got != code {
		t.Fatalf("got exit code %d, want %d", got, code)
	}
	var exiter clierr.Exiter
	if !errors.As(err, &exiter) {
		t.Fatalf("expected a clierr.Exiter, got %T", err)
	}
}
