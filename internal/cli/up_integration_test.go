//go:build integration

package cli_test

// `up`, through the real binary: the entry point, the shared router it
// builds from the environment, and the process's own signal handling. A
// fake docker answers the shared router as a hardened stack that verifies,
// and the worktree's Compose project as one that starts. Each interrupt
// case sends a real signal while a real process tree runs in one of `up`'s
// three phases — the router's own `compose up` (under the router lock), the
// worktree's `compose up` and a hook (both under the worktree lock) — and
// the process must die by that signal, with the child's whole group gone
// and the lock it held released.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/stateformat"
	"golang.org/x/sys/unix"
)

// upFakeDocker answers every docker call `up` makes. A file named hang-router
// or hang-up in state makes the router's or the worktree's `compose up`
// write its grandchild's pid to hung.pid and wait, so only a group kill stops it.
// `compose logs` prints a line, then follows: it records its own pid in
// follower.pid and a grandchild that ignores SIGINT in hung.pid, and runs
// until signalled; while a file named logs-exit exists it instead exits at
// once with the code the file holds. A file named up-fails makes the worktree's
// `compose up` fail with exit 3.
const upFakeDocker = `#!/bin/sh
state=%q
echo "$*" >> "$state/calls"
hang() { sh -c 'echo $$ > "$0/hung.pid"; exec sleep 60' "$state" & wait; exit 0; }
case " $* " in
  *" --project-name treerig-router "*" up "*) [ -e "$state/hang-router" ] && hang; exit 0 ;;
  *" --project-name treerig-router ps "*)
    for last; do :; done
    case "$last" in router) echo router-1 ;; index) echo index-1 ;; esac; exit 0 ;;
  *" compose "*" config "*) echo '{"name":"x","services":{"api":{"image":"busybox"},"mail":{"image":"busybox"}}}'; exit 0 ;;
  *" compose "*" up "*)
    [ -e "$state/up-fails" ] && { echo "boom: cannot start" >&2; exit 3; }
    [ -e "$state/hang-up" ] && hang; exit 0 ;;
  *" compose "*" logs "*)
    echo "api-1  | following"
    [ -e "$state/logs-exit" ] && exit "$(cat "$state/logs-exit")"
    sleep 60 &
    echo $! > "$state/hung.pid"
    echo $$ > "$state/follower.pid"
    exec sleep 60 ;;
esac
if [ "$1" = info ]; then
  echo '{"OSType":"linux","OperatingSystem":"Ubuntu 24.04 LTS","Name":"fake","SecurityOptions":["name=seccomp"]}'; exit 0
fi
if [ "$1" = context ]; then
  echo "unix:///var/run/docker.sock"; exit 0
fi
if [ "$1" = inspect ]; then
  [ -e "$state/$2.json" ] || exit 1
  cat "$state/$2.json"; exit 0
fi
exit 0
`

// healthyRouter writes the two inspect documents a running, healthy router
// would have, one per fake container id: the proxy in host mode serving port,
// owned by the treerig root home, as a router this treerig root started is, and
// the index beside it.
func healthyRouter(t *testing.T, state, home string, port int) {
	t.Helper()
	labels := func(service string) map[string]string {
		return map[string]string{
			"com.docker.compose.project":              router.DefaultName,
			"com.docker.compose.service":              service,
			brand.LabelNamespace + ".component":       service,
			brand.LabelNamespace + ".router-protocol": strconv.Itoa(router.Protocol),
			router.RouterHomeLabel:                    home,
		}
	}
	running := router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}}
	routerLabels := labels("router")
	routerLabels[router.RouterModeLabel] = string(router.ModeHost)
	routerLabels[router.RouterPortLabel] = strconv.Itoa(port)

	docs := map[string]router.ContainerInspect{
		"router-1": {
			Config:     router.InspectConfig{Labels: routerLabels, Cmd: []string{fmt.Sprintf("--entrypoints.web.address=127.0.0.1:%d", port)}},
			HostConfig: router.InspectHostConfig{NetworkMode: "host"},
			State:      running,
		},
		"index-1": {
			Config: router.InspectConfig{Labels: labels("index")},
			State:  running,
		},
	}
	for id, doc := range docs {
		encoded, err := json.Marshal([]router.ContainerInspect{doc})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, id+".json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type upFixture struct {
	base, checkout, controlHome, state, log string
	routerState                             layout.RouterLayout
	// seededPorts are the router state seedRouterPorts wrote.
	seededPorts router.State
	env         []string
}

// newUpFixture builds a checkout with a treerig.toml, a compose file, a hook
// script that records its stage (and can be made to hang on demand) and a
// fake docker on PATH, plus a healthy fake router, ready for `up` to be run
// against through the real binary.
func newUpFixture(t *testing.T) upFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := upFixture{
		base: base, checkout: filepath.Join(base, "checkout"), controlHome: filepath.Join(base, "control"),
		state: filepath.Join(base, "state"), log: filepath.Join(base, "hooks.log"),
	}
	bin := filepath.Join(base, "bin")
	for _, dir := range []string{filepath.Join(f.checkout, "hooks"), bin, f.state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const port = 18080
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.checkout, "treerig.toml"), "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"+
		"[service.api]\nport = 8080\n[service.mail]\nsubdomain = \"mail\"\nport = 8080\n"+
		"[hooks]\nprepare = \"./hooks/record\"\nseed = \"./hooks/record\"\n", 0o644)
	write(filepath.Join(f.checkout, "compose.yaml"), "services: {}\n", 0o644)
	// Records its stage; while hang-<stage> exists, writes its grandchild's
	// pid to hung.pid and waits.
	write(filepath.Join(f.checkout, "hooks", "record"), fmt.Sprintf(
		"#!/bin/sh\necho \"$TREERIG_STAGE\" >> %q\nif [ -e %q/hang-$TREERIG_STAGE ]; then sh -c 'echo $$ > \"$0/hung.pid\"; exec sleep 60' %q & wait; fi\n",
		f.log, f.state, f.state), 0o755)
	write(filepath.Join(bin, "docker"), fmt.Sprintf(upFakeDocker, f.state), 0o755)
	git := exec.Command("git", "init", "--initial-branch=feature/login", ".")
	git.Dir = f.checkout
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}

	f.env = append(os.Environ(),
		// TREERIG_HOME still isolates the router's own state under this
		// test's own temp directory (f.controlHome/router) rather than the
		// machine's real one; TREERIG_CONTROL_HOME (#80) points the binary
		// straight at f.controlHome for the control home itself, matching
		// the fixture paths below (layout.WorktreeLayout(f.controlHome, ...)).
		"TREERIG_HOME="+f.controlHome,
		"TREERIG_CONTROL_HOME="+f.controlHome,
		"HOME="+filepath.Join(base, "home"),
		"TREERIG_ROUTER_PORT="+strconv.Itoa(port),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	getenv := envGetenv(f.env)
	registrytest.RequireIsolatedRouterState(t, getenv)
	// The treerig root is the control home here, so it is claimed before it
	// holds a settings.toml: treerig adopts only a directory it created.
	registrytest.ClaimControlHome(t, f.controlHome)
	// The fake engine is a Linux one, which treerig on any other client OS
	// cannot place, so the mode is set: host, as the fake router above runs.
	registrytest.WriteScratchSettings(t, f.controlHome, "[router]\nmode = \"host\"\n")
	f.routerState, err = layout.RouterState(getenv)
	if err != nil {
		t.Fatal(err)
	}
	f.seededPorts = seedRouterPorts(t, f.routerState)
	healthyRouter(t, f.state, f.controlHome, port)
	return f
}

// seedRouterPorts writes the router state file with an index port and a ping
// port that were free when picked, and returns them. It is for fixtures with
// a fake router only; suites with a real router keep the product's range
// allocation. Left to itself the router picks both from a hash start that is
// the same in every treerig root (by design: the ports stay stable after a lost
// state file, and there is one router per daemon), so every fixture of every
// concurrently running test binary probes the very same two ports on each
// `up`, and one probe's brief bind makes another's port look held by another
// process (#140). Ports of its own are unlikely to meet any other probe. The
// file goes through the product's writer and is read back through its reader,
// so a state the router would ignore (a changed version, say) fails here.
func seedRouterPorts(t *testing.T, state layout.RouterLayout) router.State {
	t.Helper()
	chosen := freePorts(t, 2)
	want := router.State{Version: stateformat.RouterState, IndexPort: chosen[0], PingPort: chosen[1]}
	if err := os.MkdirAll(state.StateDir, 0o700); err != nil {
		t.Fatalf("seed router ports: create %s: %v", state.StateDir, err)
	}
	if err := files.AtomicJSON(state.StateFile, want); err != nil {
		t.Fatalf("seed router ports: write %s: %v", state.StateFile, err)
	}
	got, found, err := router.ReadState(state)
	if err != nil || !found || got != want {
		t.Fatalf("seed router ports: read back %s = %+v, found %v, %v; want %+v", state.StateFile, got, found, err, want)
	}
	return want
}

// freePorts is n distinct loopback ports that were free when picked. The
// listeners stay open together until every port is chosen, so the kernel
// cannot hand out the same one twice, and are closed before it returns.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var listeners []net.Listener
	var chosen []int
	for range n {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick a free port: %v", err)
		}
		listeners = append(listeners, listener)
		chosen = append(chosen, listener.Addr().(*net.TCPAddr).Port)
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	return chosen
}

// requireSeededRouterPorts fails unless the router state still holds the
// ports the fixture seeded, so a fixture whose `up` re-picked them fails
// loudly instead of probing the shared hash-start ports again. It is called
// after a fixture's first `up`, and only the bundle fixtures
// (newBundleFixture) call it.
func (f upFixture) requireSeededRouterPorts(t *testing.T) {
	t.Helper()
	got, found, err := router.ReadState(f.routerState)
	if err != nil || !found || got != f.seededPorts {
		t.Fatalf("router state after up = %+v, found %v, %v; want the seeded %+v", got, found, err, f.seededPorts)
	}
}

// seededPortTaken reports whether output is the router's refusal to start
// because one of f's seeded ports is in use. The router keeps a stored port
// rather than picking another, so a seeded port something else took between
// freePorts and `up` fails `up`; only that failure, naming that port, is
// worth retrying with fresh ports.
func (f upFixture) seededPortTaken(output string) bool {
	if !strings.Contains(output, "a port it needs is in use") {
		return false
	}
	for _, port := range []int{f.seededPorts.IndexPort, f.seededPorts.PingPort} {
		if strings.Contains(output, fmt.Sprintf("127.0.0.1:%d ", port)) {
			return true
		}
	}
	return false
}

// command builds binary to run in f's checkout with f's environment, ready
// for the caller to add args and run it.
func (f upFixture) command(binary string, args ...string) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.Dir = f.checkout
	cmd.Env = f.env
	return cmd
}

// calls returns the fake docker's recorded invocations, one per line, in
// the order they were made.
func (f upFixture) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// stages returns the hook stages the "record" script logged, in the order
// they ran; an absent log (no hook ran yet) is not an error.
func (f upFixture) stages(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// requireReleased checks the lock at path is free now, and that its holder
// record says it was released rather than abandoned.
func requireReleased(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("%s is still held: %v", path, err)
	}
	holder, err := os.ReadFile(path + ".holder")
	if err != nil || !strings.Contains(string(holder), `"releasedAt"`) {
		t.Fatalf("holder record = %s, %v", holder, err)
	}
}

// worktreeLock is the worktree lock of the fixture's one registration.
func (f upFixture) worktreeLock(t *testing.T) string {
	t.Helper()
	read := func(name string) string {
		return registrytest.ReadIdentityMarker(t, filepath.Join(f.checkout, ".git", brand.Name, name))
	}
	return layout.WorktreeLayout(f.controlHome, read("repository-id"), read("worktree-id")).LockPath
}

func TestUpThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)

	t.Run("starts only the services it is given, and prints their URLs on stdout", func(t *testing.T) {
		f := newUpFixture(t)
		cmd := f.command(binary, "up", "api")
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr

		if err := cmd.Run(); err != nil {
			t.Fatalf("up: %v\n%s\n%s", err, stderr.String(), strings.Join(f.calls(t), "\n"))
		}

		const want = "api   http://feature-login.acme.localhost:18080\nmail  http://mail.feature-login.acme.localhost:18080\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
		for _, line := range []string{
			"[treerig] Starting api for feature-login.acme.localhost.\n",
			"[treerig] feature-login.acme.localhost is up.\n",
		} {
			if !strings.Contains(stderr.String(), line) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), line)
			}
		}
		var routerUp, worktreeUp []string
		for _, call := range f.calls(t) {
			switch {
			case strings.Contains(call, "--project-name treerig-router ") && strings.Contains(call, " up "):
				routerUp = append(routerUp, call)
			case strings.HasPrefix(call, "compose ") && strings.Contains(call, " up "):
				worktreeUp = append(worktreeUp, call)
			}
		}
		if len(routerUp) != 1 || !strings.Contains(routerUp[0], "-f "+f.routerState.ComposeFile+" ") {
			t.Fatalf("router starts = %q, want one, from %s", routerUp, f.routerState.ComposeFile)
		}
		if len(worktreeUp) != 1 || !strings.HasSuffix(worktreeUp[0], fmt.Sprintf(" --wait-timeout %d api", backend.DefaultWaitTimeoutSeconds)) {
			t.Fatalf("worktree starts = %q, want one, of api alone", worktreeUp)
		}
		if got := f.stages(t); strings.Join(got, " ") != "prepare seed" {
			t.Fatalf("stages = %v", got)
		}
		requireReleased(t, f.routerState.LockPath)
		requireReleased(t, f.worktreeLock(t))
	})

	cases := []struct {
		name string
		// hang is the file that makes one phase wait for the signal.
		hang string
		// stages is the hooks that must have started.
		stages string
		// lock is the lock that phase held.
		lock func(f upFixture, t *testing.T) string
	}{
		{"the router's own start", "hang-router", "", func(f upFixture, _ *testing.T) string { return f.routerState.LockPath }},
		{"compose up", "hang-up", "", upFixture.worktreeLock},
		{"a hook", "hang-prepare", "prepare", upFixture.worktreeLock},
	}
	for _, tc := range cases {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(fmt.Sprintf("dies by %v during %s, with the child's group gone and the lock released", sig, tc.name), func(t *testing.T) {
				f := newUpFixture(t)
				if err := os.WriteFile(filepath.Join(f.state, tc.hang), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := f.command(binary, "up")
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
					t.Fatalf("the child's group survived: %d is alive", hung)
				}
				requireReleased(t, tc.lock(f, t))
				if got := strings.Join(f.stages(t), " "); got != tc.stages {
					t.Fatalf("stages = %q, want %q", got, tc.stages)
				}
			})
		}
	}
}
