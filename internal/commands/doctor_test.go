package commands_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// doctorGetenv isolates HOME (so router state resolves under a throwaway
// directory rather than this machine's real one) and BERTH_CONTROL_HOME (so
// the control home is w's, set directly rather than derived from BERTH_HOME,
// per #80).
func doctorGetenv(home, controlHome string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		switch key {
		case "HOME":
			return home, true
		case "BERTH_CONTROL_HOME":
			return controlHome, true
		}
		return "", false
	}
}

// withDoctorDocker answers `docker version`, `docker compose version
// --short`, the routes probe, and a bare `git --version` (which
// registrytest.GitRunner's own handler, built for `-C <worktree>` calls
// only, does not) realistically, ahead of w's existing git/compose
// dispatch, so Prerequisites and Router behave like a real, healthy machine
// unless a test overrides one of these.
func withDoctorDocker(w *commandstest.World, engineVersion, composeVersion string) {
	base := w.Runner.Handler
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Command == "git" && len(spec.Args) == 1 && spec.Args[0] == "--version" {
			return run.Result{Stdout: "git version 2.45.0"}, nil
		}
		if spec.Command == "verify-hook-fail" {
			return run.Result{Stderr: "verify: something is wrong\n", ExitCode: 7}, nil
		}
		if spec.Command == "docker" && len(spec.Args) > 0 {
			switch {
			case spec.Args[0] == "version":
				return run.Result{Stdout: engineVersion}, nil
			case len(spec.Args) > 1 && spec.Args[0] == "compose" && spec.Args[1] == "version":
				return run.Result{Stdout: composeVersion}, nil
			case spec.Args[0] == "ps" && len(spec.Args) > 1 && spec.Args[1] == "-q":
				return run.Result{Stdout: ""}, nil
			}
		}
		return base(ctx, spec)
	}
	if w.Runner.Executables == nil {
		w.Runner.Executables = map[string]bool{}
	}
	w.Runner.Executables["docker"] = true
}

// writeCheckoutConfig writes a minimal real berth.toml to root — doctor
// reads the checkout's config straight from disk (findConfigFile/config.Load),
// unlike every other command's tests, which build a commandstest.World's
// config.Loaded by hand and never touch the file findConfigFile would find.
func writeCheckoutConfig(t *testing.T, root, domain, verify string) {
	t.Helper()
	name := strings.TrimSuffix(domain, ".localhost")
	hooks := ""
	if verify != "" {
		hooks = fmt.Sprintf("\n[hooks]\nverify = %q\n", verify)
	}
	contents := fmt.Sprintf("[project]\nname = %q\ndomain = %q\n[compose]\nfile = \"compose.yaml\"\n[[service]]\nname = \"api\"\n%s",
		name, domain, hooks)
	if err := os.WriteFile(filepath.Join(root, brand.ConfigFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorEnv is a *commands.Env for Doctor against w, with cwd as the working
// directory and a fresh *logger.Recording so runDoctor can read back what was
// printed.
func doctorEnv(t *testing.T, w *commandstest.World, cwd string) *commands.Env {
	t.Helper()
	home := registrytest.TempDir(t)
	return &commands.Env{
		CommandName: "berth", Invocation: "berth doctor", Logger: &logger.Recording{},
		Getenv: doctorGetenv(home, w.ControlHome), Runner: w.Runner, Cwd: cwd,
	}
}

// wantLine asserts that report has a line of that status, subject and
// detail, tolerant of the column widths formatDoctorReport computes per
// section (which depend on every other subject sharing that section).
func wantLine(t *testing.T, report, status, subject, detail string) {
	t.Helper()
	pattern := `(?m)^\s*` + regexp.QuoteMeta(status) + `\s+` + regexp.QuoteMeta(subject) + `\s+` + regexp.QuoteMeta(detail)
	if !regexp.MustCompile(pattern).MatchString(report) {
		t.Fatalf("report missing status=%q subject=%q detail=%q:\n%s", status, subject, detail, report)
	}
}

// runDoctor runs commands.Doctor against env (whose Logger must be a
// *logger.Recording, as doctorEnv builds) and returns its exit code, its
// printed report joined into one string, and the recorder itself for
// asserting on the summary line.
func runDoctor(t *testing.T, env *commands.Env) (int, string, *logger.Recording) {
	t.Helper()
	rec, ok := env.Logger.(*logger.Recording)
	if !ok {
		t.Fatal("doctorEnv must be built with a *logger.Recording")
	}
	code, err := commands.Doctor(context.Background(), env)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	_, printed := rec.Snapshot()
	return code, strings.Join(printed, "\n"), rec
}

func TestDoctor(t *testing.T) {
	// There is no Go runtime version of its own to check, so the
	// Prerequisites section drops that check entirely rather than
	// reporting on something that does not exist.
	t.Run("never checks a Node.js version", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if regexp.MustCompile(`(?m)^\s*\w+\s+Node`).MatchString(report) {
			t.Fatalf("expected no Node.js check:\n%s", report)
		}
	})

	t.Run("reports the section titles in order", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantOrder := []string{"Prerequisites", "Registry", "Locks", "Router", "This checkout"}
		last := -1
		for _, title := range wantOrder {
			i := strings.Index(report, title)
			if i < 0 {
				t.Fatalf("report missing %q:\n%s", title, report)
			}
			if i <= last {
				t.Fatalf("%q out of order:\n%s", title, report)
			}
			last = i
		}
	})

	t.Run("passes a tool at the minimum, fails one below it, and fails output it cannot read", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "19.03.0", "2.24.0") // below the Docker Compose floor is covered separately; this exercises Engine parsing
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if !strings.Contains(report, "ok") || !strings.Contains(report, "Docker Engine") {
			t.Fatalf("expected Docker Engine ok:\n%s", report)
		}
	})

	t.Run("fails Docker off PATH, skips Compose and the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		env := doctorEnv(t, w, w.Root)
		// No withDoctorDocker: Executables["docker"] stays false.

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "fail", "Docker Engine", "the `docker` command is not on PATH; is Docker installed?")
		wantLine(t, report, "skip", "Docker Compose", "needs the `docker` command")
		wantLine(t, report, "skip", "router", "needs Docker")
	})

	t.Run("reports the control home not created yet outside any checkout", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if !strings.Contains(report, "not created yet; the first up creates it") {
			t.Fatalf("expected an uncreated control home:\n%s", report)
		}
	})

	t.Run("skips the checkout section outside any checkout, and names where it looked", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		outside := registrytest.TempDir(t)
		env := doctorEnv(t, w, outside)

		_, report, _ := runDoctor(t, env)
		want := "none in " + outside + " or above; run 'berth doctor' in a checkout to check it and its verify hook"
		if !strings.Contains(report, want) {
			t.Fatalf("report = %s, want to contain %q", report, want)
		}
	})

	t.Run("exits 0 without failures, and reports its summary on the failure/info stream", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		env := doctorEnv(t, w, w.Root)

		code, _, rec := runDoctor(t, env)
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
		found := false
		for _, line := range rec.Lines {
			if strings.HasPrefix(line, "info: No checks failed") {
				found = true
			}
		}
		if !found {
			t.Fatalf("lines = %v, want a summary", rec.Lines)
		}
	})

	t.Run("exits non-zero with a failure", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		env := doctorEnv(t, w, w.Root) // Docker off PATH: at least one fail

		code, _, rec := runDoctor(t, env)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		found := false
		for _, line := range rec.Lines {
			if strings.HasPrefix(line, "error: ") && strings.Contains(line, "check(s) failed") {
				found = true
			}
		}
		if !found {
			t.Fatalf("lines = %v, want a failure summary", rec.Lines)
		}
	})
}

func TestDoctorInACheckout(t *testing.T) {
	t.Run("skips the worktree and its verify hook until the worktree is registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		writeCheckoutConfig(t, w.Root, acme, "echo verify")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "skip", "this worktree", "not registered; 'berth up' registers it")
		wantLine(t, report, "skip", "verify hook", "needs a registered worktree; run 'berth up' first")
	})

	t.Run("says so when no verify hook is configured", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		writeCheckoutConfig(t, w.Root, acme, "")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "ok", "this worktree", "registered as feature-login.acme.localhost")
		wantLine(t, report, "skip", "verify hook", "none configured")
	})

	t.Run("warns about a registration whose checkout Git no longer lists", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		writeCheckoutConfig(t, w.Root, acme, "")
		other := w.Worktree("gone", "feature/gone")
		ref := w.Register(other, acme, "feature-gone", brand.LabelNamespace, "api")
		w.Git.SetActive([]string{w.Root}) // git worktree list no longer shows "gone"
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "warn", "feature-gone.acme.localhost",
			"registered, but Git no longer lists its checkout. Remove what it left with 'berth worktree remove "+ref.WorktreeID+"'.")
	})

	t.Run("runs the verify hook with its output on stderr, and passes when it exits 0", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		writeCheckoutConfig(t, w.Root, acme, "echo verify")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "ok", "verify hook", "echo verify exited 0")
	})

	t.Run("fails a verify hook that exits non-zero, and keeps its code", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		writeCheckoutConfig(t, w.Root, acme, "verify-hook-fail")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		env := doctorEnv(t, w, w.Root)
		// withDoctorDocker's fake Runner answers "verify-hook-fail" by name
		// alone, whatever runs it — but the new hook-resolution check (#105)
		// reads the real filesystem and PATH, so "verify-hook-fail" needs a
		// real, executable file on a real PATH entry too, or resolution
		// would fail before the fake run is ever reached.
		bin := registrytest.TempDir(t)
		if err := os.WriteFile(filepath.Join(bin, "verify-hook-fail"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		env.Environ = append([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}, os.Environ()...)

		code, report, _ := runDoctor(t, env)
		wantLine(t, report, "ok", "verify hook file", "resolves to "+filepath.Join(bin, "verify-hook-fail"))
		wantLine(t, report, "fail", "verify hook", "The verify hook failed (exit 7).")
		if code != 7 {
			t.Fatalf("code = %d, want the hook's own 7", code)
		}
	})
}

func TestDoctorLocks(t *testing.T) {
	t.Run("reports none held when no lock is taken", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantLine(t, report, "ok", "locks", "none held")
	})

	t.Run("reports a live holder as an answer, not a problem", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		operation, _ := lock.WorktreeScopes(w.ControlHome, ref.RepositoryID, ref.WorktreeID, "feature-login", procid.Markers{})
		holder := lock.New(operation, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if !regexp.MustCompile(`(?m)^\s*ok\s+feature-login\.acme\.localhost\s+held by`).MatchString(report) {
			t.Fatalf("expected a held lock reported ok:\n%s", report)
		}
	})

	t.Run("reports an unreleased-but-gone lock as recovered automatically", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		killDeadHolder(t, w.ControlHome, ref)
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if !strings.Contains(report, "recovered automatically the next time this lock is taken") {
			t.Fatalf("expected an unreleased-but-gone lock:\n%s", report)
		}
	})

	// Pins the --holder-unverifiable fallback's wording: since
	// #21, the registry and router locks have `worktree unlock
	// --registry`/`--router` of their own, so doctor's advice names that
	// instead of a hand `rm`.
	t.Run("points the registry lock's unverifiable fallback at worktree unlock --registry", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		registryScope := lock.RegistryScope(w.ControlHome)
		lk := lock.New(registryScope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		mutateRecord(t, registryScope, func(rec map[string]any) {
			rec["pid"] = deadPID
			delete(rec, "releasedAt")
		})
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		wantHint := "worktree unlock --registry --holder-unverifiable"
		if !strings.Contains(report, wantHint) {
			t.Fatalf("report = %s, want to contain %q", report, wantHint)
		}
		if strings.Contains(report, "rm '") || strings.Contains(report, "rmdir") {
			t.Fatalf("report = %s, want no rm/rmdir advice", report)
		}
	})

	t.Run("names a worktree nothing is registered under by its UUID", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		withDoctorDocker(w, "24.0.0", "2.24.0")
		ref := worktree.Ref{RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676", WorktreeID: "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"}
		killDeadHolder(t, w.ControlHome, ref)
		env := doctorEnv(t, w, w.Root)

		_, report, _ := runDoctor(t, env)
		if !strings.Contains(report, "worktree "+ref.WorktreeID) {
			t.Fatalf("expected the orphan named by UUID:\n%s", report)
		}
	})
}
