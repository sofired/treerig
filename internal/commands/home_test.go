package commands_test

// A table-driven pinning test for the refusal home.go's checkedHome adds
// (#80): whatever BERTH_HOME names, no command may derive a control home or
// a router state directory from it when it is the filesystem root, this
// process's own home directory, or anything above it. Exercised through
// every entry point that reaches resolveControlHome/resolveRouterState
// today (RouterUp, Doctor, ConfigValidate and both branches of
// UnlockGlobal), so a future call site that bypasses them — the gap an
// earlier round of review found and this test closed — would need to newly
// invent its own way past this test too, not just skip the ones already
// here.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
)

// requireBerthHomeRefusal fails t unless err is files.AssertSafeHome's own
// refusal, matched by wording rather than merely non-nil: several of these
// entry points can fail for other reasons too, and only this message proves
// BERTH_HOME is what stopped it.
func requireBerthHomeRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "Refusing to use") || !strings.Contains(err.Error(), "BERTH_HOME") {
		t.Fatalf("expected a BERTH_HOME refusal, got: %v", err)
	}
}

// noRunner fails the test outright if anything ever calls it: none of these
// four entry points should reach a runner at all once BERTH_HOME is
// refused, so a regression that lets one slip past the refusal and go on to
// touch git or Docker is caught here rather than by whatever that call
// happens to do next.
type noRunner struct{ t *testing.T }

func (n noRunner) Run(context.Context, run.Spec) (run.Result, error) {
	n.t.Helper()
	n.t.Fatal("unexpected Run call: BERTH_HOME should have been refused before anything ran")
	return run.Result{}, nil
}

func (n noRunner) Which(string) bool {
	n.t.Helper()
	n.t.Fatal("unexpected Which call: BERTH_HOME should have been refused before anything ran")
	return false
}

func TestBerthHomeRefusesUnsafeRoots(t *testing.T) {
	// checkedHome (home.go) computes userHome via layout.HomeDir(getenv) —
	// this test's own Getenv, not a direct read of the real environment —
	// and hands it to files.AssertSafeHome; so fakeHome, returned for
	// "HOME" below, is what every case's BERTH_HOME is checked against, not
	// this machine's real home directory. No t.Setenv is needed for that:
	// as of review round 3, AssertAdoptableControlHome no longer reads the
	// real HOME itself either (files.go's processHomeDir is gone) — it
	// takes userHome as a parameter, the same way AssertSafeHome always
	// has — and this test never reaches it anyway, since checkedHome's own
	// AssertSafeHome check always refuses first.
	fakeHome := registrytest.TempDir(t)

	cases := []struct {
		name string
		home string
	}{
		{"the process's own home directory", fakeHome},
		{"the parent of the process's own home directory", filepath.Dir(fakeHome)},
		{"the filesystem root", string(filepath.Separator)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) (string, bool) {
				switch key {
				case "BERTH_HOME":
					return tc.home, true
				case "HOME":
					return fakeHome, true
				}
				return "", false
			}

			t.Run("RouterUp", func(t *testing.T) {
				env := &commands.Env{
					CommandName: "berth", Invocation: "berth router up",
					Logger: &logger.Recording{}, Getenv: getenv, Runner: noRunner{t},
				}
				requireBerthHomeRefusal(t, commands.RouterUp(context.Background(), env))
			})

			t.Run("Doctor", func(t *testing.T) {
				env := &commands.Env{
					CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv,
					Cwd: registrytest.TempDir(t), Runner: noRunner{t},
				}
				_, err := commands.Doctor(context.Background(), env)
				requireBerthHomeRefusal(t, err)
			})

			t.Run("ConfigValidate", func(t *testing.T) {
				dir := registrytest.TempDir(t)
				config := filepath.Join(dir, "berth.toml")
				toml := "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"
				if err := os.WriteFile(config, []byte(toml), 0o644); err != nil {
					t.Fatal(err)
				}
				env := &commands.Env{
					CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv,
					Cwd: dir, ConfigPath: config, Runner: noRunner{t},
				}
				requireBerthHomeRefusal(t, commands.ConfigValidate(context.Background(), env))
			})

			t.Run("UnlockGlobal --registry", func(t *testing.T) {
				env := &commands.Env{CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv, Runner: noRunner{t}}
				_, err := commands.UnlockGlobal(context.Background(), env, commands.UnlockOptions{Registry: true})
				requireBerthHomeRefusal(t, err)
			})

			t.Run("UnlockGlobal --router", func(t *testing.T) {
				env := &commands.Env{CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv, Runner: noRunner{t}}
				_, err := commands.UnlockGlobal(context.Background(), env, commands.UnlockOptions{Router: true})
				requireBerthHomeRefusal(t, err)
			})
		})
	}
}
