//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run/osrun"
	"golang.org/x/sys/unix"
)

// freeTCPPort asks the kernel for a free loopback port, for a test router
// that must not collide with a real one already bound to 80.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Interrupting a mutation, through the real binary: the process must die by
// the signal it was sent, only after the child it was waiting on has been
// stopped with its whole process group, and it must leave the worktree lock
// released rather than held or abandoned.

// buildBinary compiles cmd/berth to a temporary path and returns it, so
// these tests exercise the real process (its signal handling and exit
// behaviour) rather than calling cli.Main in-process.
func buildBinary(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "berth")
	build := exec.Command("go", "build", "-o", binary, "./cmd/berth")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary
}

// fakeDocker answers as a daemon holding one container of the worktree,
// and hangs in `compose down` — in a grandchild, so only a group kill stops it.
func fakeDocker(t *testing.T, bin, state string, repositoryID, worktreeID string) {
	t.Helper()
	project := layout.ComposeProjectName(repositoryID, worktreeID)
	script := fmt.Sprintf(`#!/bin/sh
state=%q
case " $* " in
  *" compose "*" down "*) sh -c 'echo $$ > "$0/grandchild.pid"; exec sleep 60' "$state" & echo $$ > "$state/down.pid"; wait; exit 0 ;;
  *" --filter "*) case "$1" in ps) case " $* " in *"label=dev.berth.shared=true"*) exit 0 ;; esac; [ -e "$state/c1" ] && echo c1 ;; esac; exit 0 ;;
  *" --format "*) printf 'true\t%s\t%s\t%s\tapi\n'; exit 0 ;;
  *" rm "*) rm -f "$state/c1"; exit 0 ;;
esac
exit 0
`, state, project, repositoryID, worktreeID)
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
	for _, verb := range [][]string{{"worktree", "remove", "--yes"}, {"down"}} {
		verb := verb
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			sig := sig
			t.Run(strings.Join(verb[:len(verb)-min(1, len(verb)-1)], " ")+" "+sig.String(), func(t *testing.T) {
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
				if err := os.WriteFile(filepath.Join(checkout, "berth.toml"), []byte("[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"), 0o644); err != nil {
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
					"slug": "feature-login", "domain": "acme.localhost", "labelNamespace": "dev.berth", "initialization": "complete"})
				if err := os.WriteFile(paths.RecordPath, append(record, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				fakeDocker(t, bin, state, identity.RepositoryID, identity.WorktreeID)

				// Both verbs here remove route files and refresh the index
				// page, which live in the shared router's state directory:
				// isolate it the way internal/commands' own newWalk fixture
				// does, or this real binary writes to the machine's real one.
				// The router's own state now lives under BERTH_HOME alone
				// (#80) — HOME and XDG_STATE_HOME no longer matter to it —
				// so BERTH_HOME=controlHome is what isolates it below, the
				// same override the spawned binary itself gets.
				routerPort := freeTCPPort(t)
				getenv := func(key string) (string, bool) {
					switch key {
					case "BERTH_HOME", "BERTH_CONTROL_HOME":
						return controlHome, true
					case "BERTH_ROUTER_PORT":
						return strconv.Itoa(routerPort), true
					}
					return "", false
				}
				registrytest.RequireIsolatedRouterState(t, getenv)

				cmd := exec.Command(binary, verb...)
				cmd.Dir = checkout
				// BERTH_CONTROL_HOME (#80), not just BERTH_HOME, points the
				// binary straight at the fixture written above
				// (layout.WorktreeLayout(controlHome, ...)); BERTH_HOME is
				// left set too, so the router's own state (best-effort
				// index refresh) still lands under this test's own temp
				// directory rather than this machine's real one.
				cmd.Env = append(os.Environ(),
					"BERTH_HOME="+controlHome, "BERTH_CONTROL_HOME="+controlHome,
					"BERTH_ROUTER_PORT="+strconv.Itoa(routerPort),
					"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				var stderr strings.Builder
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				// Before waiting on anything: a berth that never gets as far as
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
			})
		}
	}
}
