package router_test

// Every fixture and expectation uses router.Protocol symbolically, without
// a literal protocol number baked in anywhere.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
)

// testProject and testNS are the default router's own Compose project name
// and label namespace (router.DefaultName and brand.LabelNamespace), spelled
// out literally so a fixture can build them without importing brand.
const (
	testProject = "berth-router"
	testNS      = "dev.berth"
)

// Index and ping ports a seeded state file holds in these tests. They sit
// in registrytest.PortRange, never in the default allocation range.
const (
	seedIndexPort = 30500
	seedPingPort  = 30501
)

// linuxInfo is `docker info --format '{{json .}}'` of a native rootful Linux
// engine, with the field values a real one reports.
const linuxInfo = `{"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]}`

// localSocket is the endpoint `docker context inspect` reports for the
// default context.
const localSocket = "unix:///var/run/docker.sock"

/* --------------------------------------------------------------- the fake */

// call is one recorded invocation of dockerFake.Run.
type call struct {
	args []string
	env  []string
}

// dockerFake answers the compose, info, context, ps and inspect calls the
// router makes; docs is what `docker inspect` would return.
type dockerFake struct {
	mu    sync.Mutex
	calls []call
	ids   map[string]string
	docs  map[string]router.ContainerInspect

	// info is what `docker info --format {{json .}}` prints.
	info string
	// infoExitCode and infoStderr make `docker info` fail.
	infoExitCode int
	infoStderr   string
	// endpoint is what `docker context inspect` prints; endpointExitCode
	// makes it fail.
	endpoint         string
	endpointExitCode int
	// publishers maps a host port to the names of the containers `docker ps
	// --filter publish=<port>` lists.
	publishers map[string][]string
	// logs is what `compose logs` prints.
	logs string

	upExitCode   int
	upStderr     string
	downExitCode int
	downStderr   string
	// downTimedOut makes `compose down` answer as a timeout instead of a
	// clean exit: a timed-out down must still report through run.Failure
	// (exit code 124), not the "did not stop cleanly" wording a plain
	// refusal gets.
	downTimedOut bool
	// psExitCode/psStderr is what `compose ps` says when Docker itself is the problem.
	psExitCode int
	psStderr   string
	// dockerMissing pretends `docker` is not installed at all.
	dockerMissing bool
	// onUp is called on every successful `up`, so a test can make the start itself bring containers back.
	onUp func()
}

// newDockerFake is a dockerFake on a native Linux engine with a healthy
// stack already "running": every service has a container id, and its own
// docs entry is left for the caller to set.
func newDockerFake() *dockerFake {
	return &dockerFake{
		ids:        map[string]string{"router": "router-1", "index": "index-1"},
		docs:       map[string]router.ContainerInspect{},
		info:       linuxInfo,
		endpoint:   localSocket,
		publishers: map[string][]string{},
	}
}

// containsArg reports whether want is one of args.
func containsArg(args []string, want string) bool { return slices.Contains(args, want) }

// Run answers one `docker` call, the way run.Runner requires.
func (d *dockerFake) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	d.mu.Lock()
	d.calls = append(d.calls, call{args: append([]string(nil), spec.Args...), env: append([]string(nil), spec.Env...)})
	missing := d.dockerMissing
	d.mu.Unlock()
	if missing {
		return run.Result{}, &run.StartError{Command: spec.Command, Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127}
	}

	args := spec.Args
	compose := len(args) > 0 && args[0] == "compose"
	switch {
	case len(args) > 0 && args[0] == "info":
		d.mu.Lock()
		defer d.mu.Unlock()
		return run.Result{Stdout: d.info, Stderr: d.infoStderr, ExitCode: d.infoExitCode}, nil
	case len(args) > 1 && args[0] == "context" && args[1] == "inspect":
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.endpointExitCode != 0 {
			return run.Result{Stderr: "context not found", ExitCode: d.endpointExitCode}, nil
		}
		return run.Result{Stdout: d.endpoint + "\n"}, nil
	case len(args) > 0 && args[0] == "ps":
		d.mu.Lock()
		defer d.mu.Unlock()
		var names []string
		for _, arg := range args {
			if port, ok := strings.CutPrefix(arg, "publish="); ok {
				names = d.publishers[port]
			}
		}
		return run.Result{Stdout: strings.Join(names, "\n")}, nil
	case compose && containsArg(args, "ps"):
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.psExitCode != 0 {
			return run.Result{Stderr: d.psStderr, ExitCode: d.psExitCode}, nil
		}
		return run.Result{Stdout: d.ids[args[len(args)-1]]}, nil
	case compose && containsArg(args, "up"):
		d.mu.Lock()
		upExit, upStderr, onUp := d.upExitCode, d.upStderr, d.onUp
		d.mu.Unlock()
		// A failed start brings nothing back in this fake; a leftover container is staged by hand.
		if upExit == 0 && onUp != nil {
			onUp()
		}
		return run.Result{Stderr: upStderr, ExitCode: upExit}, nil
	case compose && containsArg(args, "logs"):
		d.mu.Lock()
		defer d.mu.Unlock()
		return run.Result{Stdout: d.logs}, nil
	case compose && containsArg(args, "down"):
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.downTimedOut {
			return run.Result{Stderr: d.downStderr, ExitCode: 1, TimedOut: true}, nil
		}
		return run.Result{Stderr: d.downStderr, ExitCode: d.downExitCode}, nil
	case compose:
		return run.Result{}, nil
	case len(args) > 0 && args[0] == "inspect":
		d.mu.Lock()
		doc, present := d.docs[args[1]]
		d.mu.Unlock()
		if !present {
			return run.Result{Stderr: "no such object", ExitCode: 1}, nil
		}
		encoded, err := json.Marshal([]router.ContainerInspect{doc})
		if err != nil {
			return run.Result{}, err
		}
		return run.Result{Stdout: string(encoded)}, nil
	default:
		return run.Result{}, nil
	}
}

// Which always answers true: this fake never needs `docker` to be missing
// except through dockerMissing.
func (d *dockerFake) Which(string) bool { return true }

// snapshotCalls is every call recorded so far, safe to read while Run still runs.
func (d *dockerFake) snapshotCalls() []call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]call(nil), d.calls...)
}

// setDoc is what `docker inspect id` answers from now on.
func (d *dockerFake) setDoc(id string, doc router.ContainerInspect) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs[id] = doc
}

// deleteDoc makes `docker inspect id` answer "no such object".
func (d *dockerFake) deleteDoc(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.docs, id)
}

// setIDs replaces the containers `compose ps` knows.
func (d *dockerFake) setIDs(ids map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = ids
}

/* --------------------------------------------------------------- fixtures */

// testLabels is the component and protocol labels, plus Compose's own,
// every container in a healthy default-named stack carries, including an
// empty RouterHomeLabel — present, as Stack always leaves it, but empty,
// matching every fixture's own default Options.Home ("") in this package,
// so an ordinary Ensure test is not itself a foreign-router refusal; a test
// of ownership itself overrides or deletes it.
func testLabels(service string) map[string]string {
	return map[string]string{
		"com.docker.compose.project": testProject,
		"com.docker.compose.service": service,
		testNS + ".component":        service,
		testNS + ".router-protocol":  strconv.Itoa(router.Protocol),
		router.RouterHomeLabel:       "",
	}
}

// stackOptions are the options a healthy stack of mode and port is built from
// with the seeded index and ping ports.
func stackOptions(mode router.Mode, port int) router.StackOptions {
	return router.StackOptions{Mode: mode, Port: port, IndexPort: seedIndexPort, PingPort: seedPingPort, LabelNamespace: testNS}
}

// routerDoc is what Docker reports for a router started exactly as the
// stack asks, in mode on port.
func routerDoc(mode router.Mode, port int) router.ContainerInspect {
	labels := testLabels("router")
	labels[router.RouterModeLabel] = string(mode)
	labels[router.RouterPortLabel] = strconv.Itoa(port)
	doc := router.ContainerInspect{
		Config: router.InspectConfig{Cmd: router.RouterCommand(stackOptions(mode, port)), Labels: labels},
		State:  router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
	if mode == router.ModeHost {
		doc.HostConfig.NetworkMode = "host"
		return doc
	}
	doc.NetworkSettings.Ports = map[string][]router.InspectPortBinding{
		"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
	}
	return doc
}

// indexDoc is what Docker reports for an index container started exactly as
// the stack asks.
func indexDoc() router.ContainerInspect {
	return router.ContainerInspect{
		Config: router.InspectConfig{Cmd: append([]string(nil), router.IndexCommand...), Labels: testLabels("index")},
		NetworkSettings: router.InspectNetworkSettings{Ports: map[string][]router.InspectPortBinding{
			"8080/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(seedIndexPort)}},
		}},
		State: router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
}

/* ------------------------------------------------------------------ world */

// newState is a layout.RouterLayout rooted directly at dir, bypassing
// layout.RouterState's own HOME resolution — dir is whatever a test already
// has (a symlink, say), not derived from an environment.
func newState(dir string) layout.RouterLayout {
	indexDir := filepath.Join(dir, "index")
	return layout.RouterLayout{
		StateDir:    dir,
		ComposeFile: filepath.Join(dir, "compose.yaml"),
		DynamicDir:  filepath.Join(dir, "dynamic"),
		IndexDir:    indexDir,
		IndexFile:   filepath.Join(indexDir, "index.html"),
		LockPath:    filepath.Join(dir, "lock"),
		StateFile:   filepath.Join(dir, "state.json"),
	}
}

// testWorld is one hermetic router.Router plus its fake docker and logger.
type testWorld struct {
	t           *testing.T
	Dir         string
	ControlHome string
	State       layout.RouterLayout
	Fake        *dockerFake
	Logger      *logger.Recording
	Router      *router.Router
	Probe       registrytest.FreeProbe
	Port        int
}

// setHealthy stages docs for both services exactly as the stack asks, in
// mode, under their default fake ids.
func (w *testWorld) setHealthy(mode router.Mode) {
	w.Fake.setDoc("router-1", routerDoc(mode, w.Port))
	w.Fake.setDoc("index-1", indexDoc())
}

// seedState writes the router state file with the seeded index and ping
// ports, as an earlier start would have left it.
func (w *testWorld) seedState() {
	w.t.Helper()
	if err := os.MkdirAll(w.State.StateDir, 0o700); err != nil {
		w.t.Fatal(err)
	}
	data, err := json.Marshal(router.State{Version: 1, IndexPort: seedIndexPort, PingPort: seedPingPort})
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(w.State.StateFile, data, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// newWorldOpts builds a world with a healthy host-mode stack running on
// port and the state file seeded; existing reuses a state directory, and
// extra can override the Router's options (a custom Environ, a mode
// override, Registrations).
func newWorldOpts(t *testing.T, existing string, port int, extra func(*router.Options)) *testWorld {
	t.Helper()
	dir := existing
	if dir == "" {
		dir = registrytest.TempDir(t)
	}
	state := newState(dir)
	fake := newDockerFake()
	rec := &logger.Recording{}
	probe := registrytest.FreeProbe{Busy: map[int]bool{}}
	w := &testWorld{
		t: t, Dir: dir, ControlHome: registrytest.TempDir(t), State: state, Fake: fake, Logger: rec,
		Probe: probe, Port: port,
	}
	w.setHealthy(router.ModeHost)
	w.seedState()

	opts := router.Options{
		State: state, Port: port, Runner: fake, Logger: rec, CommandName: "duke",
		ControlHome: w.ControlHome, PortRange: registrytest.PortRange, Probe: probe, ClientOS: "linux",
	}
	if extra != nil {
		extra(&opts)
	}
	svc, err := router.New(opts)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	w.Router = svc
	return w
}

// newWorld is newWorldOpts with the default Router options.
func newWorld(t *testing.T, port int, existing string) *testWorld {
	t.Helper()
	return newWorldOpts(t, existing, port, nil)
}

// stopped makes the world's router not running — what `router down` leaves
// — and has a start bring the containers back in mode, as a real `up` would.
func stopped(t *testing.T, w *testWorld, mode router.Mode) {
	t.Helper()
	w.Fake.setIDs(map[string]string{})
	w.Fake.mu.Lock()
	w.Fake.onUp = func() {
		w.Fake.setIDs(map[string]string{"router": "router-1", "index": "index-1"})
		w.setHealthy(mode)
	}
	w.Fake.mu.Unlock()
}

// ups is every `compose … up …` call so far, as raw argv.
func (w *testWorld) ups() [][]string {
	var result [][]string
	for _, c := range w.Fake.snapshotCalls() {
		if len(c.args) > 0 && c.args[0] == "compose" && containsArg(c.args, "up") {
			result = append(result, c.args)
		}
	}
	return result
}

// firstUp is the first `compose … up …` call so far, or the zero call if
// there was none.
func (w *testWorld) firstUp() call {
	for _, c := range w.Fake.snapshotCalls() {
		if len(c.args) > 0 && c.args[0] == "compose" && containsArg(c.args, "up") {
			return c
		}
	}
	return call{}
}

// fileMode is file's permission bits.
func fileMode(t *testing.T, file string) os.FileMode {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat %s: %v", file, err)
	}
	return info.Mode().Perm()
}

// linesWithPrefix is every line of lines starting with prefix.
func linesWithPrefix(lines []string, prefix string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// containsSubstring reports whether any line of lines contains substr.
func containsSubstring(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasLine reports whether lines holds exactly want.
func hasLine(lines []string, want string) bool { return slices.Contains(lines, want) }
