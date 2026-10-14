//go:build integration

package cli_test

// `backup` and `restore`, through the real binary (#10): a registered
// fixture worktree whose fake docker reports its services running, hooks that
// record what they were given, and a real signal sent while one runs.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// bundleWrapperDocker answers `compose ps` for the worktree's project with one
// running service until `compose down` has been seen, and hands every other
// call to the fake docker `up` tests use.
const bundleWrapperDocker = `#!/bin/sh
state=%q
case " $* " in
  *" --project-name berth-router "*) exec "$0-fake" "$@" ;;
  *" compose "*" down "*) touch "$state/down"; exec "$0-fake" "$@" ;;
  *" compose "*" up "*) rm -f "$state/down"; exec "$0-fake" "$@" ;;
  *" compose "*" ps "*)
    [ -e "$state/down" ] || echo '{"Service":"api","State":"running"}'
    exit 0 ;;
esac
exec "$0-fake" "$@"
`

// bundleHook is the backup and restore hook: it records its stage, argument
// and working directory, speaks on both streams, hangs while hang-<stage>
// exists, and exits with the number in exit-<stage>.
const bundleHook = `#!/bin/sh
STATE=%q
echo "$BERTH_STAGE|$1|$(pwd)|$#" >> "$STATE/bundle.log"
echo "out:$BERTH_STAGE"
echo "err:$BERTH_STAGE" >&2
echo "env|$COMPOSE_PROJECT_NAME|$BERTH_COMPOSE_PROJECT|$COMPOSE_FILE" >> "$STATE/bundle.env"
[ -d "$1" ] && echo data > "$1/data"
if [ -e "$STATE/hang-$BERTH_STAGE" ]; then
  sh -c 'echo $$ > "$0/hung.pid"; exec sleep 60' "$STATE" &
  wait
fi
if [ -e "$STATE/exit-$BERTH_STAGE" ]; then exit "$(cat "$STATE/exit-$BERTH_STAGE")"; fi
exit 0
`

// newBundleFixture is a worktree that has been through `up`, whose berth.toml
// declares the backup and restore hooks when hooked, and whose docker reports
// a running service until `down`.
func newBundleFixture(t *testing.T, binary string, hooked bool) upFixture {
	t.Helper()
	f := newUpFixture(t)
	docker := filepath.Join(f.base, "bin", "docker")
	if err := os.Rename(docker, docker+"-fake"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docker, []byte(fmt.Sprintf(bundleWrapperDocker, f.state)), 0o755); err != nil {
		t.Fatal(err)
	}
	if hooked {
		if err := os.WriteFile(filepath.Join(f.checkout, "hooks", "bundle"), []byte(fmt.Sprintf(bundleHook, f.state)), 0o755); err != nil {
			t.Fatal(err)
		}
		toml, err := os.OpenFile(filepath.Join(f.checkout, "berth.toml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer toml.Close()
		if _, err := toml.WriteString("backup = \"./hooks/bundle\"\nrestore = \"./hooks/bundle\"\n"); err != nil {
			t.Fatal(err)
		}
	}
	output, err := f.command(binary, "up").CombinedOutput()
	if err != nil && f.seededPortTaken(string(output)) {
		// Something took a seeded port in the moment between freePorts
		// closing it and `up` probing it; pick a fresh pair, once.
		f.seededPorts = seedRouterPorts(t, f.routerState)
		output, err = f.command(binary, "up").CombinedOutput()
	}
	if err != nil {
		t.Fatalf("up: %v\n%s", err, output)
	}
	f.requireSeededRouterPorts(t)
	return f
}

// repositoryID is the fixture repository's UUID, from its identity markers.
func (f upFixture) repositoryID(t *testing.T) string {
	t.Helper()
	return registrytest.ReadIdentityMarker(t, filepath.Join(f.checkout, ".git", "berth", "repository-id"))
}

// bundleLog is what the hooks recorded, one entry per run.
func (f upFixture) bundleLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, "bundle.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func TestBackupAndRestoreThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)
	f := newBundleFixture(t, binary, true)
	nested := filepath.Join(f.base, "bundles")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("backup runs the hook with an absolute bundle directory taken from the working directory, in the project root", func(t *testing.T) {
		dir := filepath.Join(nested, "first")
		cmd := f.command(binary, "backup", "../../bundles/first")
		cmd.Dir = filepath.Join(f.checkout, "hooks")
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("backup: %v\n%s", err, errOut.String())
		}
		if out.String() != "out:backup\n" {
			t.Errorf("stdout = %q", out.String())
		}
		if !strings.Contains(errOut.String(), "err:backup\n") || !strings.Contains(errOut.String(), "Backed up feature-login.acme.localhost to "+dir+".\n") {
			t.Errorf("stderr = %q", errOut.String())
		}
		log := f.bundleLog(t)
		if want := "backup|" + dir + "|" + f.checkout + "|1"; log[len(log)-1] != want {
			t.Errorf("hook saw %q, want %q", log[len(log)-1], want)
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("bundle directory: %v, %v; want mode 0700", info, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".incomplete")); err == nil {
			t.Error("the .incomplete marker survived a successful backup")
		}
		env, err := os.ReadFile(filepath.Join(f.state, "bundle.env"))
		if err != nil {
			t.Fatal(err)
		}
		project := layout.ComposeProjectName(f.repositoryID(t), f.worktreeID(t))
		if want := "env|" + project + "|" + project + "|" + filepath.Join(f.checkout, "compose.yaml") + "\n"; string(env) != want {
			t.Errorf("hook environment = %q, want %q", env, want)
		}
	})

	t.Run("backup without a directory writes under the worktree's home", func(t *testing.T) {
		if _, stderr, code := runFixture(t, f, binary, nil, "backup"); code != 0 {
			t.Fatalf("code = %d\n%s", code, stderr)
		}
		backups := layout.WorktreeLayout(f.controlHome, f.repositoryID(t), f.worktreeID(t)).BackupsDir
		entries, err := os.ReadDir(backups)
		if err != nil || len(entries) != 1 {
			t.Fatalf("backups = %v, %v", entries, err)
		}
		if _, err := time.Parse("20060102T150405Z", entries[0].Name()); err != nil {
			t.Errorf("directory name %q is not a timestamp: %v", entries[0].Name(), err)
		}
	})

	t.Run("backup refuses a directory that holds anything, running nothing", func(t *testing.T) {
		before := len(f.bundleLog(t))
		_, stderr, code := runFixture(t, f, binary, nil, "backup", filepath.Join(nested, "first"))
		if want := "[berth] ERROR: " + filepath.Join(nested, "first") + " is not empty; give 'berth backup' a new or empty directory.\n"; stderr != want || code != 1 {
			t.Fatalf("stderr = %q, code = %d, want %q", stderr, code, want)
		}
		if len(f.bundleLog(t)) != before {
			t.Fatal("the hook ran")
		}
	})

	t.Run("restore runs the hook with the existing directory", func(t *testing.T) {
		dir := filepath.Join(nested, "first")
		stdout, stderr, code := runFixture(t, f, binary, nil, "restore", dir)
		if stdout != "out:restore\n" || code != 0 || !strings.Contains(stderr, "Restored feature-login.acme.localhost from "+dir+".\n") {
			t.Fatalf("stdout = %q, stderr = %q, code = %d", stdout, stderr, code)
		}
		log := f.bundleLog(t)
		if want := "restore|" + dir + "|" + f.checkout + "|1"; log[len(log)-1] != want {
			t.Errorf("hook saw %q, want %q", log[len(log)-1], want)
		}
	})

	t.Run("restore refuses a missing directory, and no directory at all", func(t *testing.T) {
		missing := filepath.Join(nested, "missing")
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"restore", missing}, "[berth] ERROR: " + missing + " does not exist; restore needs an existing bundle directory.\n"},
			{[]string{"restore"}, "[berth] ERROR: Restore needs a bundle directory: berth restore <dir>.\n"},
		} {
			stdout, stderr, code := runFixture(t, f, binary, nil, tc.args...)
			if stdout != "" || stderr != tc.want || code != 1 {
				t.Errorf("%v: stdout = %q, stderr = %q, code = %d, want %q", tc.args, stdout, stderr, code, tc.want)
			}
		}
	})

	t.Run("restore refuses a bundle a backup did not finish, running nothing", func(t *testing.T) {
		dir := filepath.Join(nested, "unfinished")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".incomplete"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(f.bundleLog(t))
		_, stderr, code := runFixture(t, f, binary, nil, "restore", dir)
		if code != 1 || !strings.Contains(stderr, "was left incomplete by a backup that did not finish") || len(f.bundleLog(t)) != before {
			t.Fatalf("stderr = %q, code = %d", stderr, code)
		}
	})

	t.Run("exits with the hook's own status", func(t *testing.T) {
		for stage, status := range map[string]int{"backup": 7, "restore": 9} {
			if err := os.WriteFile(filepath.Join(f.state, "exit-"+stage), []byte(strconv.Itoa(status)), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(nested, "status-"+stage)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			_, stderr, code := runFixture(t, f, binary, nil, stage, dir)
			if want := fmt.Sprintf("[berth] ERROR: The %s hook failed (exit %d).\n", stage, status); code != status || !strings.Contains(stderr, want) {
				t.Errorf("%s: code = %d, stderr = %q, want %d and %q", stage, code, stderr, status, want)
			}
			_ = os.Remove(filepath.Join(f.state, "exit-"+stage))
		}
	})

	t.Run("refuses inside its own worktree's hooks before taking the lock", func(t *testing.T) {
		started := time.Now()
		_, stderr, code := runFixture(t, f, binary, []string{"BERTH_WORKTREE_ID=" + f.worktreeID(t)}, "backup", filepath.Join(nested, "nested"))
		if code != 1 || !strings.Contains(stderr, "runs inside feature-login.acme.localhost's own hooks or commands, which may hold its lock") {
			t.Fatalf("stderr = %q, code = %d", stderr, code)
		}
		if time.Since(started) > 10*time.Second {
			t.Fatal("it waited on the lock")
		}
	})

	t.Run("refuses once the worktree is down", func(t *testing.T) {
		down := newBundleFixture(t, binary, true)
		if output, err := down.command(binary, "down").CombinedOutput(); err != nil {
			t.Fatalf("down: %v\n%s", err, output)
		}
		for _, stage := range []string{"backup", "restore"} {
			_, stderr, code := runFixture(t, down, binary, nil, stage, down.base)
			want := "[berth] ERROR: feature-login.acme.localhost has no running service, and the " + stage + " hook needs one. Run 'berth up' first.\n"
			if stderr != want || code != 1 {
				t.Errorf("%s: stderr = %q, code = %d, want %q", stage, stderr, code, want)
			}
		}
		if len(down.bundleLog(t)) != 0 {
			t.Fatal("a hook ran")
		}
	})

	t.Run("refuses a hook that is not configured, and a worktree that is not registered", func(t *testing.T) {
		bare := newBundleFixture(t, binary, false)
		_, stderr, code := runFixture(t, bare, binary, nil, "backup", filepath.Join(bare.base, "out"))
		if want := "[berth] ERROR: No backup hook is configured: add backup under [hooks] in berth.toml.\n"; stderr != want || code != 1 {
			t.Errorf("stderr = %q, code = %d, want %q", stderr, code, want)
		}
		if _, err := os.Stat(filepath.Join(bare.base, "out")); err == nil {
			t.Error("created the directory")
		}

		fresh := newUpFixture(t)
		if err := os.WriteFile(filepath.Join(fresh.checkout, "hooks", "bundle"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		toml, err := os.OpenFile(filepath.Join(fresh.checkout, "berth.toml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer toml.Close()
		if _, err := toml.WriteString("backup = \"./hooks/bundle\"\n"); err != nil {
			t.Fatal(err)
		}
		_, stderr, code = runFixture(t, fresh, binary, nil, "backup", filepath.Join(fresh.base, "out"))
		if want := "[berth] ERROR: This worktree is not registered. Run 'berth up' first.\n"; stderr != want || code != 1 {
			t.Errorf("stderr = %q, code = %d, want %q", stderr, code, want)
		}
	})
}

// A signal reaches the hook's whole process group, berth dies by it, and the
// worktree's lock is released.
func TestBackupSignalsThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)
	// So berth gets the default SIGINT, SIGTERM and SIGHUP action, even under nohup.
	catchTerminationSignals(t)
	for _, stage := range []string{"backup", "restore"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
			t.Run(fmt.Sprintf("%s dies by %v with the hook's group gone and the lock released", stage, sig), func(t *testing.T) {
				f := newBundleFixture(t, binary, true)
				if err := os.WriteFile(filepath.Join(f.state, "hang-"+stage), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(f.base, "bundle")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := f.command(binary, stage, dir)
				var stderr strings.Builder
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
				hungFile := filepath.Join(f.state, "hung.pid")
				t.Cleanup(func() {
					if data, err := os.ReadFile(hungFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
							_ = unix.Kill(pid, syscall.SIGKILL)
						}
					}
				})
				hung, _ := strconv.Atoi(waitForFile(t, hungFile))

				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err := cmd.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("exit = %v\n%s", err, stderr.String())
				}
				if status := exit.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != sig {
					t.Fatalf("status = %v, want death by %v\n%s", status, sig, stderr.String())
				}
				if alive(hung) {
					t.Fatalf("the hook's group survived: %d is alive", hung)
				}
				requireReleased(t, f.worktreeLock(t))
				if stage == "backup" {
					if _, err := os.Stat(filepath.Join(dir, ".incomplete")); err != nil {
						t.Errorf("an interrupted backup lost its marker: %v", err)
					}
				}
			})
		}
	}
}
