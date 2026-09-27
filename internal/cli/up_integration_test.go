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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"golang.org/x/sys/unix"
)

// upFakeDocker answers every docker call `up` makes. A file named hang-router
// or hang-up in state makes the router's or the worktree's `compose up`
// write its grandchild's pid to hung.pid and wait, so only a group kill stops it.
const upFakeDocker = `#!/bin/sh
state=%q
echo "$*" >> "$state/calls"
hang() { sh -c 'echo $$ > "$0/hung.pid"; exec sleep 60' "$state" & wait; exit 0; }
case " $* " in
  *" --project-name berth-router "*" up "*) [ -e "$state/hang-router" ] && hang; exit 0 ;;
  *" --project-name berth-router ps "*)
    for last; do :; done
    case "$last" in router) echo router-1 ;; index) echo index-1 ;; esac; exit 0 ;;
  *" compose "*" config "*) echo '{"name":"x","services":{"api":{"image":"busybox"},"mail":{"image":"busybox"}}}'; exit 0 ;;
  *" compose "*" up "*) [ -e "$state/hang-up" ] && hang; exit 0 ;;
esac
if [ "$1" = inspect ]; then
  [ -e "$state/$2.json" ] || exit 1
  cat "$state/$2.json"; exit 0
fi
exit 0
`

// healthyRouter writes the three inspect documents a router started exactly
// as the stack asks would have, one per fake container id, owned by the
// berth root home, as a router this berth root started is.
func healthyRouter(t *testing.T, state, home string, routerState layout.RouterLayout, port int) {
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
	hardened := func(capAdd ...string) router.InspectHostConfig {
		return router.InspectHostConfig{
			CapAdd: append([]string{}, capAdd...), CapDrop: []string{"ALL"},
			ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
		}
	}
	running := router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}}
	networks := func(names ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for _, name := range names {
			m[name] = json.RawMessage("{}")
		}
		return m
	}

	indexLabels := labels("index")
	for key, value := range router.IndexRouteLabels(router.DefaultName).Map() {
		indexLabels[key] = value
	}

	docs := map[string]router.ContainerInspect{
		"router-1": {
			Config:     router.InspectConfig{Cmd: router.RouterCommand(), Entrypoint: router.RouterImageEntrypoint, Image: router.RouterImage, Labels: labels("router")},
			HostConfig: hardened("CAP_NET_BIND_SERVICE"),
			Mounts:     []router.InspectMount{{Type: "bind", Source: routerState.DynamicDir, Destination: router.DynamicConfigMount}},
			NetworkSettings: router.InspectNetworkSettings{
				Networks: networks(router.DefaultName),
				Ports:    map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}}},
			},
			State: running,
		},
		"index-1": {
			Config:          router.InspectConfig{Cmd: append([]string(nil), router.IndexCommand...), Entrypoint: router.IndexImageEntrypoint, Image: router.IndexImage, Labels: indexLabels},
			HostConfig:      hardened(),
			Mounts:          []router.InspectMount{{Type: "bind", Source: routerState.IndexDir, Destination: router.IndexMount}},
			NetworkSettings: router.InspectNetworkSettings{Networks: networks(router.DefaultName)},
			State:           running,
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
	env                                     []string
}

// newUpFixture builds a checkout with a berth.toml, a compose file, a hook
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
	write(filepath.Join(f.checkout, "berth.toml"), "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"+
		"[[service]]\nname = \"api\"\nport = 8080\n[[service]]\nname = \"mail\"\nsubdomain = \"mail\"\nport = 8080\n"+
		"[hooks]\nprepare = \"./hooks/record\"\nseed = \"./hooks/record\"\n", 0o644)
	write(filepath.Join(f.checkout, "compose.yaml"), "services: {}\n", 0o644)
	// Records its stage; while hang-<stage> exists, writes its grandchild's
	// pid to hung.pid and waits.
	write(filepath.Join(f.checkout, "hooks", "record"), fmt.Sprintf(
		"#!/bin/sh\necho \"$BERTH_STAGE\" >> %q\nif [ -e %q/hang-$BERTH_STAGE ]; then sh -c 'echo $$ > \"$0/hung.pid\"; exec sleep 60' %q & wait; fi\n",
		f.log, f.state, f.state), 0o755)
	write(filepath.Join(bin, "docker"), fmt.Sprintf(upFakeDocker, f.state), 0o755)
	git := exec.Command("git", "init", "--initial-branch=feature/login", ".")
	git.Dir = f.checkout
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}

	f.env = append(os.Environ(),
		// BERTH_HOME still isolates the router's own state under this
		// test's own temp directory (f.controlHome/router) rather than the
		// machine's real one; BERTH_CONTROL_HOME (#80) points the binary
		// straight at f.controlHome for the control home itself, matching
		// the fixture paths below (layout.WorktreeLayout(f.controlHome, ...)).
		"BERTH_HOME="+f.controlHome,
		"BERTH_CONTROL_HOME="+f.controlHome,
		"HOME="+filepath.Join(base, "home"),
		"BERTH_ROUTER_PORT="+strconv.Itoa(port),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	getenv := envGetenv(f.env)
	registrytest.RequireIsolatedRouterState(t, getenv)
	f.routerState, err = layout.RouterState(getenv)
	if err != nil {
		t.Fatal(err)
	}
	healthyRouter(t, f.state, f.controlHome, f.routerState, port)
	return f
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
		data, err := os.ReadFile(filepath.Join(f.checkout, ".git", brand.Name, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
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
			"[berth] Starting api for feature-login.acme.localhost.\n",
			"[berth] feature-login.acme.localhost is up.\n",
		} {
			if !strings.Contains(stderr.String(), line) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), line)
			}
		}
		var routerUp, worktreeUp []string
		for _, call := range f.calls(t) {
			switch {
			case strings.Contains(call, "--project-name berth-router ") && strings.Contains(call, " up "):
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
