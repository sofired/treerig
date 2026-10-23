package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

// initEnv is an Env for `init` in cwd, whose Git answers --show-toplevel with
// toplevel (a non-empty string) or "fatal: not a git repository" (empty).
func initEnv(t *testing.T, cwd, toplevel string) (*Env, *logger.Recording, *fakerun.Runner) {
	t.Helper()
	runner := &fakerun.Runner{Handler: func(_ context.Context, spec run.Spec) (run.Result, error) {
		if toplevel == "" {
			return run.Result{ExitCode: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git"}, nil
		}
		return run.Result{Stdout: toplevel + "\n"}, nil
	}}
	env, rec := testEnv(t, filepath.Join(cwd, "treerig.toml"), registrytest.TempDir(t))
	env.ConfigPath = ""
	env.Cwd = cwd
	env.Runner = runner
	return env, rec, runner
}

// TestInit pins `init`: where it writes, what it refuses and what it says.
func TestInit(t *testing.T) {
	ctx := context.Background()

	t.Run("writes a starter at the git toplevel that config validate accepts", func(t *testing.T) {
		root := registrytest.TempDir(t)
		sub := filepath.Join(root, "pkg", "deep")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		env, rec, runner := initEnv(t, sub, root)

		if err := Init(ctx, env); err != nil {
			t.Fatalf("Init: %v", err)
		}

		want := "git -c core.fsmonitor=false -c core.hooksPath=/dev/null -c safe.bareRepository=explicit -C " + sub + " rev-parse --show-toplevel"
		if got := runner.Argv(); len(got) != 1 || got[0] != want {
			t.Errorf("git calls = %q, want %q", got, want)
		}
		if _, err := os.Stat(filepath.Join(sub, "treerig.toml")); err == nil {
			t.Error("wrote treerig.toml in the working directory, want the repository root")
		}
		written, err := os.ReadFile(filepath.Join(root, "treerig.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(written), "#:schema https://raw.githubusercontent.com/sofired/treerig/main/schema/treerig.schema.json\n") {
			t.Errorf("starter does not open with the schema directive:\n%s", written)
		}
		info, err := os.Stat(filepath.Join(root, "treerig.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v, want 0644", info.Mode().Perm())
		}

		lines, printed := rec.Snapshot()
		if len(printed) != 0 {
			t.Errorf("stdout = %q, want nothing", printed)
		}
		if len(lines) != 3 || lines[0] != "info: Wrote "+filepath.Join(root, "treerig.toml")+"." ||
			!strings.HasPrefix(lines[1], "warn: No Compose file at "+root) ||
			lines[2] != "info: Edit treerig.toml, then run 'treerig config validate'." {
			t.Errorf("notices = %q", lines)
		}

		// The pinned pair: what init writes, config validate accepts.
		validate, vrec := testEnv(t, filepath.Join(root, "treerig.toml"), registrytest.TempDir(t))
		if err := ConfigValidate(ctx, validate); err != nil {
			t.Fatalf("ConfigValidate on the starter: %v", err)
		}
		_, out := vrec.Snapshot()
		if len(out) < 2 || !strings.Contains(out[0], "treerig.toml is valid.") {
			t.Errorf("validate output = %q", out)
		}
		name := filepath.Base(root)
		if !strings.Contains(out[1], "project "+name+" · domain "+name+".localhost · 0 service(s) · 0 hook(s)") {
			t.Errorf("validate summary = %q, want the project named after the directory", out[1])
		}
	})

	t.Run("asks git in the C locale, keeping the rest of the environment", func(t *testing.T) {
		root := registrytest.TempDir(t)
		env, _, runner := initEnv(t, root, root)
		env.Environ = []string{"PATH=/usr/bin", "LANG=de_DE.UTF-8"}

		if err := Init(ctx, env); err != nil {
			t.Fatal(err)
		}

		calls := runner.Calls()
		if len(calls) != 1 || strings.Join(calls[0].Env, " ") != "PATH=/usr/bin LANG=de_DE.UTF-8 LC_ALL=C" {
			t.Errorf("git environment = %q, want the process environment plus LC_ALL=C", calls[0].Env)
		}
		if len(env.Environ) != 2 {
			t.Errorf("env.Environ = %q, want it left unchanged", env.Environ)
		}
	})

	t.Run("drops the variables that point git at another repository, and only those", func(t *testing.T) {
		root := registrytest.TempDir(t)
		env, _, runner := initEnv(t, root, root)
		env.Environ = []string{
			"PATH=/usr/bin", "GIT_DIR=/other/.git", "GIT_WORK_TREE=/other", "GIT_COMMON_DIR=/other/.git",
			"GIT_INDEX_FILE=/other/.git/index", "GIT_OBJECT_DIRECTORY=/other/.git/objects",
			"GIT_IMPLICIT_WORK_TREE=0", "GIT_PREFIX=sub/",
			"GIT_CEILING_DIRECTORIES=/home", "GIT_CONFIG_COUNT=1", "GIT_DIRECTORY_X=keep", "HOME=/h",
		}

		if err := Init(ctx, env); err != nil {
			t.Fatal(err)
		}

		want := "PATH=/usr/bin GIT_CEILING_DIRECTORIES=/home GIT_CONFIG_COUNT=1 GIT_DIRECTORY_X=keep HOME=/h LC_ALL=C"
		if got := strings.Join(runner.Calls()[0].Env, " "); got != want {
			t.Errorf("git environment = %q, want %q", got, want)
		}
	})

	t.Run("names the Compose file it finds, in Compose's own order, and does not warn", func(t *testing.T) {
		for _, tc := range []struct {
			present []string
			want    string
		}{
			{[]string{"compose.yaml"}, "compose.yaml"},
			{[]string{"compose.yml"}, "compose.yml"},
			{[]string{"docker-compose.yml"}, "docker-compose.yml"},
			{[]string{"docker-compose.yaml"}, "docker-compose.yaml"},
			{[]string{"docker-compose.yml", "compose.yml", "docker-compose.yaml"}, "compose.yml"},
			{[]string{"docker-compose.yml", "docker-compose.yaml"}, "docker-compose.yaml"},
		} {
			root := registrytest.TempDir(t)
			for _, name := range tc.present {
				if err := os.WriteFile(filepath.Join(root, name), []byte("services: {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env, rec, _ := initEnv(t, root, root)

			if err := Init(ctx, env); err != nil {
				t.Fatalf("%v: Init: %v", tc.present, err)
			}

			written, _ := os.ReadFile(filepath.Join(root, "treerig.toml"))
			if !strings.Contains(string(written), "\nfile = \""+tc.want+"\"\n") {
				t.Errorf("%v: starter's [compose] file is not %s:\n%s", tc.present, tc.want, written)
			}
			lines, _ := rec.Snapshot()
			for _, line := range lines {
				if strings.HasPrefix(line, "warn:") {
					t.Errorf("%v: unexpected warning %q", tc.present, line)
				}
			}
			validate, _ := testEnv(t, filepath.Join(root, "treerig.toml"), registrytest.TempDir(t))
			if err := ConfigValidate(ctx, validate); err != nil {
				t.Errorf("%v: ConfigValidate: %v", tc.present, err)
			}
		}
	})

	t.Run("ignores a directory named like a Compose file", func(t *testing.T) {
		root := registrytest.TempDir(t)
		if err := os.Mkdir(filepath.Join(root, "compose.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		env, rec, _ := initEnv(t, root, root)
		if err := Init(ctx, env); err != nil {
			t.Fatal(err)
		}
		lines, _ := rec.Snapshot()
		if len(lines) != 3 || !strings.HasPrefix(lines[1], "warn: No Compose file") {
			t.Errorf("notices = %q, want the missing-Compose-file warning", lines)
		}
	})

	t.Run("refuses to overwrite an existing treerig.toml and leaves it untouched", func(t *testing.T) {
		root := registrytest.TempDir(t)
		existing := filepath.Join(root, "treerig.toml")
		if err := os.WriteFile(existing, []byte("# mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env, rec, _ := initEnv(t, root, root)

		err := Init(ctx, env)

		want := existing + " already exists; not overwriting it. Edit it, or remove it first to start over."
		if err == nil || err.Error() != want || !clierr.Expected(err) || clierr.Code(err) != 1 {
			t.Fatalf("Init error = %v (code %d), want an expected exit-1 error %q", err, clierr.Code(err), want)
		}
		if got, _ := os.ReadFile(existing); string(got) != "# mine\n" {
			t.Errorf("treerig.toml = %q, want it untouched", got)
		}
		if lines, printed := rec.Snapshot(); len(lines)+len(printed) != 0 {
			t.Errorf("printed %q %q before refusing", lines, printed)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 1 {
			t.Errorf("directory has %v, want only treerig.toml", entries)
		}
	})

	t.Run("refuses outside a git repository and writes nothing", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		env, _, _ := initEnv(t, dir, "")

		err := Init(ctx, env)

		want := dir + " is not inside a Git checkout (fatal: not a git repository (or any of the parent directories): .git). Run 'treerig init' at the root of the repository you want to manage."
		if err == nil || err.Error() != want || !clierr.Expected(err) {
			t.Fatalf("Init error = %v, want %q", err, want)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("directory has %v, want it empty", entries)
		}
	})

	t.Run("refuses --config, which it has no use for", func(t *testing.T) {
		root := registrytest.TempDir(t)
		env, _, runner := initEnv(t, root, root)
		env.ConfigPath = "/elsewhere/treerig.toml"

		err := Init(ctx, env)

		want := "'treerig init' writes treerig.toml at the root of the Git repository you run it in; it does not take --config."
		if err == nil || err.Error() != want {
			t.Fatalf("Init error = %v, want %q", err, want)
		}
		if len(runner.Calls()) != 0 {
			t.Errorf("ran %v before refusing", runner.Argv())
		}
	})

	t.Run("says to install Git, exiting 127, when git is not on PATH", func(t *testing.T) {
		root := registrytest.TempDir(t)
		env, _, runner := initEnv(t, root, root)
		runner.Handler = func(context.Context, run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: "git", Code: "ENOENT", Detail: "was not found on PATH (ENOENT).", ExitCode: 127}
		}

		err := Init(ctx, env)

		want := "Git was not found on PATH. Install Git, then run 'treerig init' again."
		if err == nil || err.Error() != want || clierr.Code(err) != 127 {
			t.Fatalf("Init error = %v (code %d), want %q with code 127", err, clierr.Code(err), want)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("directory has %v, want it empty", entries)
		}
	})

	t.Run("quotes git's own reason for any other failure instead of calling it not a checkout", func(t *testing.T) {
		root := registrytest.TempDir(t)
		env, _, runner := initEnv(t, root, root)
		runner.Handler = func(context.Context, run.Spec) (run.Result, error) {
			return run.Result{ExitCode: 128, Stderr: "fatal: detected dubious ownership in repository at '/x'\nTo add an exception run:\n"}, nil
		}

		err := Init(ctx, env)

		want := "Git could not find the root of the checkout containing " + root + ": fatal: detected dubious ownership in repository at '/x'"
		if err == nil || err.Error() != want || strings.Contains(err.Error(), "not inside") {
			t.Fatalf("Init error = %v, want %q", err, want)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("directory has %v, want it empty", entries)
		}
	})

	t.Run("keeps whitespace in the toplevel path git printed", func(t *testing.T) {
		parent := registrytest.TempDir(t)
		root := filepath.Join(parent, " spaced dir ")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		env, _, _ := initEnv(t, root, root)

		if err := Init(ctx, env); err != nil {
			t.Fatalf("Init: %v", err)
		}

		if _, err := os.Stat(filepath.Join(root, "treerig.toml")); err != nil {
			t.Errorf("treerig.toml not written in the path with surrounding spaces: %v", err)
		}
		written, _ := os.ReadFile(filepath.Join(root, "treerig.toml"))
		if !strings.Contains(string(written), "\nname = \"spaced-dir\"\n") {
			t.Errorf("project name not derived from the directory:\n%s", written)
		}
	})

	t.Run("lets only one of eight racing inits write, and neither corrupts the file", func(t *testing.T) {
		root := registrytest.TempDir(t)
		const racers = 8
		results := make(chan error, racers)
		for range racers {
			env, _, _ := initEnv(t, root, root)
			go func() { results <- Init(ctx, env) }()
		}
		wins := 0
		for range racers {
			if err := <-results; err == nil {
				wins++
			} else if !strings.Contains(err.Error(), "already exists; not overwriting it") {
				t.Errorf("unexpected error: %v", err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d inits succeeded, want exactly 1", wins)
		}
		validate, _ := testEnv(t, filepath.Join(root, "treerig.toml"), registrytest.TempDir(t))
		if err := ConfigValidate(ctx, validate); err != nil {
			t.Errorf("ConfigValidate after the race: %v", err)
		}
	})
}
