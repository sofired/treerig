package hooks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/hooks"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/worktree"
)

// applyCommand is a locked command with the default kill-after, as a bare
// string in treerig.toml would give.
func applyCommand(line string) config.Command {
	return config.Command{Command: line, Lock: true, KillAfter: config.DefaultCommandKillAfter}
}

func TestRunCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("runs interactively from the project root, resolved like a hook, with the arguments after -- appended", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{})
		cmd := config.Command{Command: `./scripts/keycloak.sh apply "two words"`, Lock: true, KillAfter: "60s"}
		if err := hooks.RunCommand(ctx, "apply", cmd, []string{"-auto-approve", "a b"}, hctx); err != nil {
			t.Fatal(err)
		}
		calls := runner.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %v", calls)
		}
		call := calls[0]
		if call.Command != "/work/acme/scripts/keycloak.sh" {
			t.Errorf("Command = %q", call.Command)
		}
		if got := strings.Join(call.Args, "|"); got != "apply|two words|-auto-approve|a b" {
			t.Errorf("Args = %q", got)
		}
		if call.Dir != "/work/acme" {
			t.Errorf("Dir = %q", call.Dir)
		}
		if call.Output != run.Interactive {
			t.Errorf("Output = %v, want Interactive", call.Output)
		}
		if call.KillAfter != time.Minute {
			t.Errorf("KillAfter = %v, want the command's 60s", call.KillAfter)
		}
		if call.InheritLocks {
			t.Error("a command must not inherit the worktree's locks")
		}
		if call.Timeout != 0 {
			t.Errorf("Timeout = %v, want none", call.Timeout)
		}
	})

	t.Run("leaves a bare name to PATH", func(t *testing.T) {
		runner := recordingRunner(nil)
		if err := hooks.RunCommand(ctx, "plan", applyCommand("terraform plan"), nil, newContext(runner, config.Hooks{})); err != nil {
			t.Fatal(err)
		}
		if call := runner.Calls()[0]; call.Command != "terraform" || len(call.Args) != 1 || call.Args[0] != "plan" {
			t.Fatalf("call = %+v", call)
		}
	})

	t.Run("sets TREERIG_COMMAND and the worktree's variables, and never TREERIG_STAGE, inherited or not", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{})
		// As inside a hook of another worktree that runs `treerig run`.
		hctx.Environ = []string{"PATH=/usr/bin", "TREERIG_STAGE=prepare", "TREERIG_COMMAND=stale"}
		if err := hooks.RunCommand(ctx, "apply", applyCommand("./apply.sh"), nil, hctx); err != nil {
			t.Fatal(err)
		}
		env := runner.Calls()[0].Env
		for k, v := range map[string]string{
			"TREERIG_COMMAND": "apply", "TREERIG_SLUG": "feature-login", "TREERIG_WORKTREE_ID": "wt-1",
			"TREERIG_HOST": "feature-login.acme.localhost", "TREERIG_ROOT": "/work/acme", "PATH": "/usr/bin",
		} {
			if !containsEnv(env, k, v) {
				t.Errorf("want %s=%s in %v", k, v, env)
			}
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "TREERIG_STAGE=") {
				t.Errorf("TREERIG_STAGE reached the command: %v", env)
			}
		}
	})

	t.Run("hands the command TREERIG_URL and TREERIG_SERVICE_URL_<NAME> like a hook (#28)", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{})
		hctx.Routes = worktree.Routes([]config.Service{{Name: "api"}, {Name: "mail-relay", Subdomain: "mail"}}, hctx.Ref.Slug, hctx.Source.Domain, 8443)
		hctx.Environ = []string{"TREERIG_SERVICE_URL_OTHER=http://other"}
		if err := hooks.RunCommand(ctx, "apply", applyCommand("./apply.sh"), nil, hctx); err != nil {
			t.Fatal(err)
		}
		env := runner.Calls()[0].Env
		if !containsEnv(env, "TREERIG_URL", "http://feature-login.acme.localhost:8443") ||
			!containsEnv(env, "TREERIG_SERVICE_URL_MAIL_RELAY", "http://mail.feature-login.acme.localhost:8443") {
			t.Errorf("env = %v", env)
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "TREERIG_SERVICE_URL_OTHER=") {
				t.Errorf("inherited %s leaked", entry)
			}
		}
	})

	t.Run("hands the command the shared service addresses like a hook", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{})
		hctx.Shared = []worktree.SharedAddr{{Name: "mailpit", Port: 1025, Addr: "127.0.0.1:30001"}}
		if err := hooks.RunCommand(ctx, "apply", applyCommand("./apply.sh"), nil, hctx); err != nil {
			t.Fatal(err)
		}
		if env := runner.Calls()[0].Env; !containsEnv(env, "TREERIG_SHARED_ADDR_MAILPIT_1025", "127.0.0.1:30001") {
			t.Errorf("env = %v", env)
		}
	})

	t.Run("a hook never sees TREERIG_COMMAND, even inherited from a command that ran treerig", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./prepare.sh")})
		hctx.Environ = []string{"TREERIG_COMMAND=apply"}
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		env := runner.Calls()[0].Env
		if !containsEnv(env, "TREERIG_STAGE", "prepare") {
			t.Errorf("env = %v", env)
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "TREERIG_COMMAND=") {
				t.Errorf("TREERIG_COMMAND reached a hook: %v", env)
			}
		}
	})

	t.Run("prints nothing of its own: no start line, no heartbeat", func(t *testing.T) {
		rec := &logger.Recording{}
		hctx := newContext(recordingRunner(nil), config.Hooks{})
		hctx.Logger = rec
		if err := hooks.RunCommand(ctx, "plan", applyCommand("./plan.sh"), nil, hctx); err != nil {
			t.Fatal(err)
		}
		if lines, printed := rec.Snapshot(); len(lines) != 0 || len(printed) != 0 {
			t.Fatalf("logged %v, printed %v", lines, printed)
		}
	})

	t.Run("attributes a non-zero exit to the command and preserves the exit code", func(t *testing.T) {
		runner := recordingRunner(map[string]int{"/work/acme/apply.sh": 2})
		err := hooks.RunCommand(ctx, "apply", applyCommand("./apply.sh"), nil, newContext(runner, config.Hooks{}))
		requireMessage(t, err, 2, "The apply command failed (exit 2).")
	})

	t.Run("says command, not hook, when it cannot start, and keeps 127 and 126", func(t *testing.T) {
		err := hooks.RunCommand(ctx, "apply", applyCommand("./scripts/missing.sh"), nil, newContext(brokenRunner("ENOENT"), config.Hooks{}))
		requireMessage(t, err, 127,
			"The apply command could not be started: `/work/acme/scripts/missing.sh` does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).")
		err = hooks.RunCommand(ctx, "apply", applyCommand("./scripts/apply"), nil, newContext(brokenRunner("EACCES"), config.Hooks{}))
		requireMessage(t, err, 126, "The apply command could not be started: `/work/acme/scripts/apply` is not executable (EACCES).")
	})

	t.Run("reports a command a signal ended with 128 plus the signal", func(t *testing.T) {
		err := hooks.RunCommand(ctx, "apply", applyCommand("make apply"), nil, newContext(brokenRunner("SIGKILL"), config.Hooks{}))
		requireMessage(t, err, 137, "The apply command was terminated by SIGKILL.")
	})

	t.Run("refuses an empty command without running anything", func(t *testing.T) {
		runner := recordingRunner(nil)
		for _, line := range []string{"", "   ", `""`} {
			err := hooks.RunCommand(ctx, "apply", applyCommand(line), nil, newContext(runner, config.Hooks{}))
			requireMessage(t, err, 1, `Command "apply" is empty.`)
		}
		if len(runner.Calls()) != 0 {
			t.Fatalf("calls = %v", runner.Calls())
		}
	})

	t.Run("returns the runner's own error, such as a cancellation, untouched", func(t *testing.T) {
		cancelled := context.Canceled
		runner := &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) { return run.Result{}, cancelled }}
		if err := hooks.RunCommand(ctx, "apply", applyCommand("./apply.sh"), nil, newContext(runner, config.Hooks{})); err != cancelled {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestResolveCommand(t *testing.T) {
	t.Run("resolves a relative command against the root and predicts it starts", func(t *testing.T) {
		root := registrytest.TempDir(t)
		writeScript(t, root+"/plan.sh", "#!/bin/sh\n", 0o755)
		resolution, err := hooks.ResolveCommand("plan", applyCommand("./plan.sh --fast"), root, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Command != root+"/plan.sh" || len(resolution.Args) != 1 || resolution.Args[0] != "--fast" {
			t.Fatalf("resolution = %+v", resolution)
		}
	})

	t.Run("predicts 127 for a missing file, naming the path it looked for", func(t *testing.T) {
		root := registrytest.TempDir(t)
		resolution, err := hooks.ResolveCommand("plan", applyCommand("./missing.sh"), root, nil)
		var startErr *run.StartError
		if !asStartError(err, &startErr) || startErr.ExitCode != 127 || resolution.Command != root+"/missing.sh" {
			t.Fatalf("resolution = %+v, err = %v", resolution, err)
		}
	})

	t.Run("refuses an empty command as the command it is", func(t *testing.T) {
		_, err := hooks.ResolveCommand("plan", applyCommand("  "), "/root", nil)
		if err == nil || err.Error() != `Command "plan" is empty.` {
			t.Fatalf("err = %v", err)
		}
	})
}

// asStartError is errors.As for a *run.StartError.
func asStartError(err error, target **run.StartError) bool {
	return errors.As(err, target)
}
