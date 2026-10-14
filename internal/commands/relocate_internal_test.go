package commands

// Tests relocateAndReexec directly (issue #7): given a fake Executable and
// Exec (real syscall.Exec cannot be exercised in a unit test — it would
// replace the go test binary's own process), it installs a copy of the
// running binary into the content-addressed store and, unless that binary
// is already the installed copy, hands it to Exec unchanged.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// fakeEnv is a relocateAndReexec fixture: a temp berth root and a separate
// temp home directory (so checkedHome's own AssertSafeHome check has a
// userHome that neither equals nor contains the berth root, #80), a temp
// "running binary" file with known content, and a recording logger.
type fakeEnv struct {
	env *Env
	// controlHome is the value wired as BERTH_HOME — the berth root (#80),
	// not the control home itself, which nests one level under it at
	// <controlHome>/control-home; use the controlLayout method to derive
	// it the same way relocateAndReexec does.
	controlHome string
	binary      string
	log         *logger.Recording
}

// newFakeEnv writes content to a file standing in for the running binary,
// under its own directory (never controlHome, so the "still in a checkout"
// case is the default) and wires an Env whose Executable reports it, HOME
// resolves to a home directory of its own, and BERTH_HOME resolves to a
// berth root of its own.
func newFakeEnv(t *testing.T, content string) *fakeEnv {
	t.Helper()
	base := registrytest.TempDir(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	berthRoot := filepath.Join(base, "berth-root")
	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(checkout, "berth")
	if err := os.WriteFile(binary, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	log := &logger.Recording{}
	f := &fakeEnv{controlHome: berthRoot, binary: binary, log: log}
	f.env = &Env{
		Logger: log,
		Getenv: func(key string) (string, bool) {
			switch key {
			case "BERTH_HOME":
				return berthRoot, true
			case "HOME":
				return home, true
			}
			return "", false
		},
		Args:       []string{binary, "worktree", "remove", "--yes"},
		Environ:    []string{"PATH=/usr/bin"},
		Executable: func() (string, error) { return binary, nil },
	}
	return f
}

// controlLayout derives the real control home nested under f's own berth
// root (layout.ControlHomeFrom) and its layout, the same way
// relocateAndReexec itself does via checkedHome — so a test that needs to
// predict or seed a path under the store computes it the identical way,
// through whatever f.env.Getenv currently answers.
func (f *fakeEnv) controlLayout(t *testing.T) layout.Control {
	t.Helper()
	home, err := layout.ControlHomeFrom(f.controlHome, f.env.Getenv, "")
	if err != nil {
		t.Fatalf("ControlHomeFrom: %v", err)
	}
	return layout.ControlLayout(home)
}

// reexec runs relocateAndReexec with a live context and fails t on an error.
func reexec(t *testing.T, env *Env) {
	t.Helper()
	if err := relocateAndReexec(context.Background(), env); err != nil {
		t.Fatalf("relocateAndReexec = %v", err)
	}
}

// wantWarning fails t unless log holds exactly one line equal to want.
func wantWarning(t *testing.T, log *logger.Recording, want string) {
	t.Helper()
	lines, _ := log.Snapshot()
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log lines = %v, want exactly [%q]", lines, want)
	}
}

// wantWarningWrapping fails t unless log holds exactly one line that is
// prefix, then some wrapped error text this test does not pin exactly
// (an OS or filesystem error whose own wording can vary), then suffix.
func wantWarningWrapping(t *testing.T, log *logger.Recording, prefix, suffix string) {
	t.Helper()
	lines, _ := log.Snapshot()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], prefix) || !strings.HasSuffix(lines[0], suffix) {
		t.Fatalf("log lines = %v, want exactly one line with prefix %q and suffix %q", lines, prefix, suffix)
	}
}

func TestRelocateAndReexec(t *testing.T) {
	t.Run("does nothing when Executable or Exec is not wired", func(t *testing.T) {
		f := newFakeEnv(t, "a build")
		f.env.Exec = nil
		reexec(t, f.env) // Executable is set, Exec is not: still a no-op.
		if _, err := os.Stat(f.controlLayout(t).BinDir); !os.IsNotExist(err) {
			t.Fatalf("expected no bin directory, stat error: %v", err)
		}

		f2 := newFakeEnv(t, "a build")
		f2.env.Executable = nil
		f2.env.Exec = func(string, []string, []string) error { t.Fatal("Exec should not be called"); return nil }
		reexec(t, f2.env)
	})

	t.Run("installs a copy outside the checkout and hands it to Exec with argv and env unchanged", func(t *testing.T) {
		f := newFakeEnv(t, "a berth build")
		var gotArgv0 string
		var gotArgv, gotEnvv []string
		called := false
		f.env.Exec = func(argv0 string, argv, envv []string) error {
			called = true
			gotArgv0, gotArgv, gotEnvv = argv0, argv, envv
			return nil
		}

		reexec(t, f.env)

		if !called {
			t.Fatalf("Exec was not called")
		}
		control := f.controlLayout(t)
		wantPath, err := binstore.Install(control.BinDir, []byte("a berth build"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("binstore.Install: %v", err)
		}
		if gotArgv0 != wantPath {
			t.Fatalf("argv0 = %q, want the installed copy %q", gotArgv0, wantPath)
		}
		if filepath.Dir(gotArgv0) == filepath.Dir(f.binary) {
			t.Fatalf("installed copy %q is still inside the checkout %q", gotArgv0, filepath.Dir(f.binary))
		}
		if len(gotArgv) != len(f.env.Args) || gotArgv[0] != f.env.Args[0] {
			t.Fatalf("argv = %v, want %v (unchanged)", gotArgv, f.env.Args)
		}
		if len(gotEnvv) != 1 || gotEnvv[0] != "PATH=/usr/bin" {
			t.Fatalf("envv = %v, want the Env's own Environ unchanged", gotEnvv)
		}
		got, err := os.ReadFile(wantPath)
		if err != nil {
			t.Fatalf("reading installed copy: %v", err)
		}
		if string(got) != "a berth build" {
			t.Fatalf("installed copy content = %q", got)
		}
	})

	t.Run("returns the cancellation, and does not exec, when a signal arrived while the copy was installed", func(t *testing.T) {
		f := newFakeEnv(t, "interrupted")
		ctx, cancel := context.WithCancelCause(context.Background())
		interrupted := &clierr.Cancelled{Signal: syscall.SIGINT}
		// The signal lands during relocation: Exec is the first thing that
		// would notice, and it must never be reached.
		cancel(interrupted)
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec was called on a cancelled context; the interruption would be forgotten")
			return nil
		}

		err := relocateAndReexec(ctx, f.env)

		if !errors.Is(err, interrupted) {
			t.Fatalf("relocateAndReexec = %v, want the cancellation", err)
		}
	})

	t.Run("does not re-exec when already running the installed copy", func(t *testing.T) {
		f := newFakeEnv(t, "already installed")
		control := f.controlLayout(t)
		installed, err := binstore.Install(control.BinDir, []byte("already installed"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("binstore.Install: %v", err)
		}
		f.env.Executable = func() (string, error) { return installed, nil }
		f.env.Args = []string{installed, "worktree", "remove", "--yes"}
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called when already running the installed copy")
			return nil
		}

		reexec(t, f.env)
	})

	t.Run("falls back to the running binary, warning, when it cannot find its own executable path", func(t *testing.T) {
		f := newFakeEnv(t, "unreadable")
		f.env.Executable = func() (string, error) { return "", errors.New("boom") }
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called after a failure to find the running binary")
			return nil
		}

		reexec(t, f.env) // Must not panic and must not fail the caller.

		wantWarning(t, f.log, "warn: Could not find its own running binary before removing: boom; continuing with this binary.")
	})

	t.Run("falls back to the running binary, warning, when it cannot read the executable's content", func(t *testing.T) {
		f := newFakeEnv(t, "irrelevant")
		missing := filepath.Join(filepath.Dir(f.binary), "does-not-exist")
		f.env.Executable = func() (string, error) { return missing, nil }
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called after a failure to read the running binary's content")
			return nil
		}

		reexec(t, f.env)

		wantWarning(t, f.log, fmt.Sprintf("warn: Could not read its own running binary before removing: open %s: no such file or directory; continuing with this binary.", missing))
	})

	t.Run("falls back to the running binary, warning, when the control home cannot be resolved", func(t *testing.T) {
		f := newFakeEnv(t, "irrelevant")
		// checkedHome refuses the filesystem root as BERTH_HOME on every OS,
		// before anything touches the filesystem, so relocation can't
		// resolve a control home. HOME stays the fixture's own scratch
		// directory, so the refusal is about "/" and nothing else.
		previous := f.env.Getenv
		f.env.Getenv = func(key string) (string, bool) {
			if key == "BERTH_HOME" {
				return "/", true
			}
			return previous(key)
		}
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called after a failure to resolve the control home")
			return nil
		}

		reexec(t, f.env)

		wantWarningWrapping(t, f.log, "warn: Could not resolve the control home before removing: ", "; continuing with this binary.")
	})

	t.Run("falls back to the running binary, warning, when the control home cannot be secured", func(t *testing.T) {
		f := newFakeEnv(t, "irrelevant")
		// The berth root's own parent (the fixture's base) already exists;
		// putting a plain file exactly where the berth root belongs makes
		// establishControlHome's own os.MkdirAll — one level under it, at
		// <berth root>/control-home — fail, since a path cannot be
		// created through a non-directory component.
		if err := os.WriteFile(f.controlHome, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called after a failure to secure the control home")
			return nil
		}

		reexec(t, f.env)

		wantWarningWrapping(t, f.log, "warn: Could not prepare a standalone copy of itself before removing: ", "; continuing with this binary.")
	})

	t.Run("falls back to the running binary, warning, when Exec itself fails", func(t *testing.T) {
		f := newFakeEnv(t, "will fail to exec")
		f.env.Exec = func(string, []string, []string) error { return errors.New("permission denied") }

		reexec(t, f.env)

		wantWarning(t, f.log, "warn: Could not run its own standalone copy before removing: permission denied; continuing with this binary.")
	})

	t.Run("prunes synchronously before Exec, since a successful Exec never returns to run anything deferred", func(t *testing.T) {
		f := newFakeEnv(t, "current build")
		f.env.Exec = func(string, []string, []string) error { return nil }
		// Establishes the store the normal way first — an
		// AssertAdoptableControlHome-refused control home would otherwise
		// refuse a bin/ directory seeded any other way — before a stale
		// entry is added directly.
		reexec(t, f.env)

		control := f.controlLayout(t)
		stale, err := binstore.Install(control.BinDir, []byte("stale build"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("binstore.Install: %v", err)
		}
		staleTime := time.Now().Add(-(standaloneMaxAge + 24*time.Hour))
		if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
			t.Fatal(err)
		}

		// A fake Exec that mirrors "a successful exec never returns" by
		// panicking with a sentinel instead of returning at all — the way
		// a real, successful execve replaces this process and never lets
		// relocateAndReexec run anything after the call, deferred or not.
		type execCalled struct{}
		f.env.Exec = func(string, []string, []string) error { panic(execCalled{}) }

		func() {
			defer func() {
				switch r := recover().(type) {
				case execCalled:
					// Expected: Exec was reached.
				case nil:
					t.Fatalf("relocateAndReexec returned without ever reaching Exec")
				default:
					panic(r)
				}
			}()
			reexec(t, f.env)
		}()

		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("stale entry should already be pruned before Exec ran, stat error: %v", err)
		}
	})

	t.Run("prunes an old entry other than the one just installed", func(t *testing.T) {
		f := newFakeEnv(t, "current build")
		f.env.Exec = func(string, []string, []string) error { return nil }
		// One relocation establishes the store and installs "current build";
		// the store directory must exist through the normal path (an
		// AssertAdoptableControlHome-refused control home would otherwise
		// refuse a bin/ directory seeded any other way) before a second
		// entry can be added directly.
		reexec(t, f.env)

		control := f.controlLayout(t)
		current, err := binstore.Install(control.BinDir, []byte("current build"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("binstore.Install: %v", err)
		}
		stale, err := binstore.Install(control.BinDir, []byte("stale build"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("binstore.Install: %v", err)
		}
		staleTime := time.Now().Add(-(standaloneMaxAge + 24*time.Hour))
		if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
			t.Fatal(err)
		}

		// The second relocation runs from the now-installed copy (Executable
		// reports it), so it takes the already-installed branch and never
		// calls Exec again — the sweep runs synchronously either way,
		// right after Install, before that branch is even decided.
		f.env.Executable = func() (string, error) { return current, nil }
		f.env.Exec = func(string, []string, []string) error {
			t.Fatal("Exec should not be called when already running the installed copy")
			return nil
		}
		reexec(t, f.env)

		if _, err := os.Stat(current); err != nil {
			t.Fatalf("current entry was pruned: %v", err)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("expected the stale entry to be pruned, stat error: %v", err)
		}
	})
}
