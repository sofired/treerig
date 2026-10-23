//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run/osrun"
	"golang.org/x/sys/unix"
)

// catchTerminationSignals is for every test that signals a treerig binary
// (#140). It makes the binaries the test starts see the default action of
// SIGINT, SIGTERM and SIGHUP. A signal ignored when treerig starts stays
// ignored (lifecycle.Start leaves it alone, as a nohup'd process expects),
// and the test may itself be running under nohup, whose ignored SIGHUP every
// child inherits. A caught signal is reset to its default across exec, so
// catching them here hands treerig the defaults whatever this process was
// started with. The channel exists only as Notify's destination and is
// deliberately never read. While the helper is active a real Ctrl-C or
// SIGTERM sent to `go test` is swallowed, until the test's cleanup stops
// the catch.
func catchTerminationSignals(t *testing.T) {
	t.Helper()
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })
}

// freeTCPPort asks the kernel for a free loopback port, for a test router
// that must not collide with a real one already bound to 80.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	return freePorts(t, 1)[0]
}

// Interrupting a mutation, through the real binary: the process must die by
// the signal it was sent, only after the child it was waiting on has been
// stopped with its whole process group, and it must leave the worktree lock
// released rather than held or abandoned.

// buildBinary compiles cmd/treerig to a temporary path and returns it, so
// these tests exercise the real process (its signal handling and exit
// behaviour) rather than calling cli.Main in-process.
func buildBinary(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "treerig")
	build := exec.Command("go", "build", "-o", binary, "./cmd/treerig")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary
}

// hangPoint is where fakeDocker's daemon stops answering.
type hangPoint string

const (
	// hangInDown hangs `compose down`, before a removal has destroyed anything.
	hangInDown hangPoint = "down"
	// hangInReap answers `compose down` and hangs removing the container,
	// after a removal has marked the worktree pending.
	hangInReap hangPoint = "reap"
)

// fakeDocker answers as a daemon holding one container of the worktree,
// and hangs where at says — in a grandchild, so only a group kill stops it.
func fakeDocker(t *testing.T, bin, state string, repositoryID, worktreeID string, at hangPoint) {
	t.Helper()
	project := layout.ComposeProjectName(repositoryID, worktreeID)
	const hang = `sh -c 'echo $$ > "$0/grandchild.pid"; exec sleep 60' "$state" & echo $$ > "$state/down.pid"; wait; exit 0`
	down, rm := hang, `rm -f "$state/c1"; exit 0`
	if at == hangInReap {
		down, rm = "exit 0", hang
	}
	script := fmt.Sprintf(`#!/bin/sh
state=%q
case " $* " in
  *" compose "*" down "*) %s ;;
  *" --filter "*) case "$1" in ps) case " $* " in *"label=dev.treerig.shared=true"*) exit 0 ;; esac; [ -e "$state/c1" ] && echo c1 ;; esac; exit 0 ;;
  *" --format "*) printf 'true\t%s\t%s\t%s\tapi\t1\n'; exit 0 ;;
  *" rm "*) %s ;;
esac
exit 0
`, state, down, project, repositoryID, worktreeID, rm)
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "c1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitForFile polls for path to appear with non-blank content and returns
// it trimmed, failing the test if it never does within 20 seconds; used to
// synchronise with the fake docker helper's background writes.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

// alive reports whether pid is still running: it answers the null signal
// and, where /proc says so (Linux), is not a zombie. On macOS, which has no
// /proc, a zombie still counts as alive. A process whose parent
// has not reaped it yet still answers, but has done all the dying it will.
func alive(pid int) bool {
	if unix.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ...": the state is the field after the last ")".
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] != 'Z'
	}
	return true
}

func TestInterruptedMutation(t *testing.T) {
	binary := buildBinary(t)
	type scenario struct {
		verb []string
		at   hangPoint
	}
	for _, sc := range []scenario{
		{[]string{"worktree", "remove", "--yes"}, hangInDown},
		{[]string{"down"}, hangInDown},
		// Past the point where the registration is marked pending (#149).
		{[]string{"worktree", "remove", "--yes"}, hangInReap},
	} {
		verb, at := sc.verb, sc.at
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			sig := sig
			name := strings.Join(verb[:len(verb)-min(1, len(verb)-1)], " ") + " " + sig.String()
			if at == hangInReap {
				name += " while removing"
			}
			t.Run(name, func(t *testing.T) {
				base, _ := filepath.EvalSymlinks(t.TempDir())
				controlHome := filepath.Join(base, "control")
				checkout := filepath.Join(base, "checkout")
				bin := filepath.Join(base, "bin")
				state := filepath.Join(base, "state")
				for _, dir := range []string{checkout, bin, state} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(checkout, "treerig.toml"), []byte("[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				git := exec.Command("git", "init", "--initial-branch=feature/login", ".")
				git.Dir = checkout
				if output, err := git.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v\n%s", err, output)
				}
				// Registered as `up` would have: markers, a complete record, a home.
				identity, err := registry.NewIdentityService(checkout, osrun.New()).Ensure(context.Background(), checkout)
				if err != nil {
					t.Fatal(err)
				}
				paths := layout.WorktreeLayout(controlHome, identity.RepositoryID, identity.WorktreeID)
				if err := files.SecureWorktreeDirectories(paths, filepath.Join(base, "user-home")); err != nil {
					t.Fatal(err)
				}
				record, _ := json.Marshal(map[string]any{"version": 1, "repositoryId": identity.RepositoryID, "worktreeId": identity.WorktreeID,
					"slug": "feature-login", "domain": "acme.localhost", "labelNamespace": "dev.treerig", "initialization": "complete"})
				if err := os.WriteFile(paths.RecordPath, append(record, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				fakeDocker(t, bin, state, identity.RepositoryID, identity.WorktreeID, at)

				// Both verbs here remove route files and refresh the index
				// page, which live in the shared router's state directory:
				// isolate it the way internal/commands' own newWalk fixture
				// does, or this real binary writes to the machine's real one.
				// The router's own state now lives under TREERIG_HOME alone
				// (#80) — HOME and XDG_STATE_HOME no longer matter to it —
				// so TREERIG_HOME=controlHome is what isolates it below, the
				// same override the spawned binary itself gets.
				routerPort := freeTCPPort(t)
				getenv := func(key string) (string, bool) {
					switch key {
					case "TREERIG_HOME", "TREERIG_CONTROL_HOME":
						return controlHome, true
					case "TREERIG_ROUTER_PORT":
						return strconv.Itoa(routerPort), true
					}
					return "", false
				}
				registrytest.RequireIsolatedRouterState(t, getenv)

				env := append(os.Environ(),
					"TREERIG_HOME="+controlHome, "TREERIG_CONTROL_HOME="+controlHome,
					"TREERIG_ROUTER_PORT="+strconv.Itoa(routerPort),
					"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				// `worktree remove` runs only the pinned teardown binary (#146):
				// pin the build under test first. It is the same process id
				// after the gate's exec, so the signal below reaches the pinned
				// build exactly as it reached this one.
				if verb[0] == "worktree" {
					pinWithEnv(t, binary, checkout, env, "signal-test")
				}
				cmd := exec.Command(binary, verb...)
				cmd.Dir = checkout
				// TREERIG_CONTROL_HOME (#80), not just TREERIG_HOME, points the
				// binary straight at the fixture written above
				// (layout.WorktreeLayout(controlHome, ...)); TREERIG_HOME is
				// left set too, so the router's own state (best-effort
				// index refresh) still lands under this test's own temp
				// directory rather than this machine's real one.
				cmd.Env = env
				var stderr strings.Builder
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				// Before waiting on anything: a treerig that never gets as far as
				// starting docker is not left running either.
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
				grandchildFile := filepath.Join(state, "grandchild.pid")
				t.Cleanup(func() {
					if data, err := os.ReadFile(grandchildFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
							_ = unix.Kill(pid, syscall.SIGKILL)
						}
					}
				})
				grandchild, _ := strconv.Atoi(waitForFile(t, grandchildFile))

				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err = cmd.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("exit = %v\n%s", err, stderr.String())
				}
				status := exit.Sys().(syscall.WaitStatus)
				if !status.Signaled() || status.Signal() != sig {
					t.Fatalf("status = %v, want death by %v\n%s", status, sig, stderr.String())
				}
				if alive(grandchild) {
					t.Fatalf("the child's group survived: %d is alive", grandchild)
				}
				// Released: free now, and its record says so.
				lockFile, err := os.OpenFile(paths.LockPath, os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer lockFile.Close()
				if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatalf("the worktree lock is still held: %v", err)
				}
				holder, err := os.ReadFile(paths.LockPath + ".holder")
				if err != nil || !strings.Contains(string(holder), `"releasedAt"`) {
					t.Fatalf("holder record = %s, %v", holder, err)
				}
				// Nothing was destroyed past the point of interruption: the registration is still there.
				if found, _ := files.PathExists(paths.RecordPath); !found {
					t.Fatal("the registration went")
				}
				// An interruption past the pending mark says what it left (#149);
				// one before it left nothing to say.
				notice := "Interrupted (" + clierr.SignalName(sig) + ") while removing feature-login.acme.localhost. " +
					"Removed so far: nothing confirmed yet. It is still registered and now reads 'removing': " +
					"'treerig worktree remove' finishes the removal, 'treerig up' calls it off and runs the seed hook again."
				if at == hangInReap {
					if !strings.Contains(stderr.String(), notice+"\n") {
						t.Fatalf("stderr lacks the interruption notice %q:\n%s", notice, stderr.String())
					}
					var record struct {
						Initialization string `json:"initialization"`
					}
					data, err := os.ReadFile(paths.RecordPath)
					if err != nil || json.Unmarshal(data, &record) != nil || record.Initialization != "pending" {
						t.Fatalf("record = %s, %v; want it pending", data, err)
					}
				} else if strings.Contains(stderr.String(), "Interrupted (") {
					t.Fatalf("stderr has an interruption notice for a removal that had destroyed nothing:\n%s", stderr.String())
				}
			})
		}
	}
}
