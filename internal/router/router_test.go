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

// testProject, testNetwork and testNS are the default router's own name,
// network and label namespace (router.DefaultName and brand.LabelNamespace),
// spelled out literally so a fixture can build them without importing brand.
const (
	testProject = "berth-router"
	testNetwork = "berth-router"
	testNS      = "dev.berth"
)

/* --------------------------------------------------------------- the fake */

// call is one recorded invocation of dockerFake.Run.
type call struct {
	args []string
	env  []string
}

// dockerFake answers the compose/inspect calls the router makes; docs is
// what `docker inspect` would return.
type dockerFake struct {
	mu    sync.Mutex
	calls []call
	ids   map[string]string
	docs  map[string]router.ContainerInspect

	upExitCode   int
	upStderr     string
	downExitCode int
	downStderr   string
	// downTimedOut makes `compose down` answer as a timeout instead of a
	// clean exit — round 3 of #55's review: a timed-out down must not be
	// read as Compose's own "has active endpoints" refusal even if its
	// stderr happened to contain that text, and must still report through
	// run.Failure (exit code 124), not the "did not stop cleanly" wording a
	// plain refusal gets.
	downTimedOut bool
	// psExitCode/psStderr is what `compose ps` says when Docker itself is the problem.
	psExitCode int
	psStderr   string
	// dockerMissing pretends `docker` is not installed at all.
	dockerMissing bool
	// onUp is called on every `up`, so a test can make the start itself bring containers back.
	onUp func()
	// onRecreate is called when `up --force-recreate` runs, so a test can make the recreation "fix" things.
	onRecreate func()
	// networkLs is what `docker network ls` prints: the labelled worktree
	// networks, one name per line. Empty means the router labels none.
	networkLs string
	// networkLsFails makes `docker network ls` exit 1, as an unreachable daemon would.
	networkLsFails bool
	// connectExitCode/connectStderr and disconnectExitCode/disconnectStderr
	// are what `docker network connect`/`disconnect` answer; zero exit codes
	// mean success (the default a test that never sets them gets).
	connectExitCode    int
	connectStderr      string
	disconnectExitCode int
	disconnectStderr   string
}

// newDockerFake is a dockerFake with a healthy stack already "running":
// every service has a container id, and its own docs entry is left for the
// caller to set.
func newDockerFake() *dockerFake {
	return &dockerFake{
		ids:  map[string]string{"router": "router-1", "index": "index-1"},
		docs: map[string]router.ContainerInspect{},
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
	switch {
	case len(args) > 0 && args[0] == "compose" && containsArg(args, "ps"):
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.psExitCode != 0 {
			return run.Result{Stderr: d.psStderr, ExitCode: d.psExitCode}, nil
		}
		return run.Result{Stdout: d.ids[args[len(args)-1]]}, nil
	case len(args) > 0 && args[0] == "compose" && containsArg(args, "up"):
		d.mu.Lock()
		upExit, upStderr, onUp, onRecreate := d.upExitCode, d.upStderr, d.onUp, d.onRecreate
		d.mu.Unlock()
		// A failed start brings nothing back in this fake; a leftover container is staged by hand.
		if upExit == 0 && onUp != nil {
			onUp()
		}
		if containsArg(args, "--force-recreate") && onRecreate != nil {
			onRecreate()
		}
		return run.Result{Stderr: upStderr, ExitCode: upExit}, nil
	case len(args) > 0 && args[0] == "compose" && containsArg(args, "down"):
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.downTimedOut {
			return run.Result{Stderr: d.downStderr, ExitCode: 1, TimedOut: true}, nil
		}
		return run.Result{Stderr: d.downStderr, ExitCode: d.downExitCode}, nil
	case len(args) > 0 && args[0] == "compose":
		return run.Result{}, nil
	case len(args) > 1 && args[0] == "network" && args[1] == "ls":
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.networkLsFails {
			return run.Result{Stderr: "Cannot connect to the Docker daemon", ExitCode: 1}, nil
		}
		return run.Result{Stdout: d.networkLs}, nil
	case len(args) > 1 && args[0] == "network" && args[1] == "connect":
		d.mu.Lock()
		defer d.mu.Unlock()
		return run.Result{Stderr: d.connectStderr, ExitCode: d.connectExitCode}, nil
	case len(args) > 1 && args[0] == "network" && args[1] == "disconnect":
		d.mu.Lock()
		defer d.mu.Unlock()
		return run.Result{Stderr: d.disconnectStderr, ExitCode: d.disconnectExitCode}, nil
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

// clearDocs makes every `docker inspect` answer "no such object".
func (d *dockerFake) clearDocs() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs = map[string]router.ContainerInspect{}
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

// routerDoc is what Docker reports for a router started exactly as the stack
// asks, attached to only its own network — every fixture in this package
// runs with no worktree ever having brought a per-worktree router network up
// (`docker network ls --filter label=...` answers empty by default, see
// dockerFake.Run), so labelledWorktreeNetworks is always empty here too.
func routerDoc(dynamicDir string, port int) router.ContainerInspect {
	networks := map[string]json.RawMessage{testNetwork: json.RawMessage("{}")}
	return router.ContainerInspect{
		Config: router.InspectConfig{
			Cmd:        router.RouterCommand(),
			Entrypoint: router.RouterImageEntrypoint,
			Image:      router.RouterImage,
			Labels:     testLabels("router"),
		},
		HostConfig: router.InspectHostConfig{
			CapAdd: []string{"CAP_NET_BIND_SERVICE"}, CapDrop: []string{"ALL"},
			ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
		},
		Mounts: []router.InspectMount{
			{Type: "bind", Source: dynamicDir, Destination: router.DynamicConfigMount, RW: false},
		},
		NetworkSettings: router.InspectNetworkSettings{
			Networks: networks,
			Ports: map[string][]router.InspectPortBinding{
				"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
			},
		},
		State: router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
}

// initTrue is the HostConfig.Init Docker reports for a container started
// with an init process.
var initTrue = true

// indexDoc is what Docker reports for an index container started exactly as
// the stack asks.
func indexDoc(indexDir string) router.ContainerInspect {
	labels := testLabels("index")
	for key, value := range router.IndexRouteLabels(testNetwork).Map() {
		labels[key] = value
	}
	return router.ContainerInspect{
		Config: router.InspectConfig{
			Cmd:        append([]string(nil), router.IndexCommand...),
			Entrypoint: router.IndexImageEntrypoint,
			Image:      router.IndexImage,
			Labels:     labels,
		},
		HostConfig: router.InspectHostConfig{
			CapAdd: []string{}, CapDrop: []string{"ALL"}, Init: &initTrue, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
		},
		Mounts: []router.InspectMount{
			{Type: "bind", Source: indexDir, Destination: router.IndexMount, RW: false},
		},
		NetworkSettings: router.InspectNetworkSettings{
			Networks: map[string]json.RawMessage{testNetwork: json.RawMessage("{}")},
			Ports:    map[string][]router.InspectPortBinding{},
		},
		State: router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
}

/* ------------------------------------------------------------------ world */

// newState is a layout.RouterLayout rooted directly at dir, bypassing
// layout.RouterState's own HOME/XDG resolution — dir is whatever a test
// already has (a symlink, say), not derived from an environment.
func newState(dir string) layout.RouterLayout {
	indexDir := filepath.Join(dir, "index")
	return layout.RouterLayout{
		StateDir:    dir,
		ComposeFile: filepath.Join(dir, "compose.yaml"),
		DynamicDir:  filepath.Join(dir, "dynamic"),
		IndexDir:    indexDir,
		IndexFile:   filepath.Join(indexDir, "index.html"),
		LockPath:    filepath.Join(dir, "lock"),
	}
}

// testWorld is one hermetic router.Router plus its fake docker and logger.
type testWorld struct {
	t      *testing.T
	Dir    string
	State  layout.RouterLayout
	Fake   *dockerFake
	Logger *logger.Recording
	Router *router.Router
	Port   int
}

// setHealthy stages docs for both services exactly as the stack asks, under
// their default fake ids.
func (w *testWorld) setHealthy() {
	w.Fake.setDoc("router-1", routerDoc(w.State.DynamicDir, w.Port))
	w.Fake.setDoc("index-1", indexDoc(w.State.IndexDir))
}

// newWorldOpts builds a world; existing reuses a state directory (a second
// control home run against the same machine-wide router), and extra can
// override the Router's options (e.g. a custom Environ).
func newWorldOpts(t *testing.T, existing string, port int, extra func(*router.Options)) *testWorld {
	t.Helper()
	dir := existing
	if dir == "" {
		dir = registrytest.TempDir(t)
	}
	state := newState(dir)
	fake := newDockerFake()
	rec := &logger.Recording{}
	w := &testWorld{t: t, Dir: dir, State: state, Fake: fake, Logger: rec, Port: port}
	w.setHealthy()

	opts := router.Options{State: state, Port: port, Runner: fake, Logger: rec, CommandName: "duke"}
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

// taken is a control home whose router containers are gone — what `router
// down` leaves. Starting it again brings the containers back, as a real
// `up` would.
func taken(t *testing.T, w *testWorld) {
	t.Helper()
	w.Fake.mu.Lock()
	w.Fake.ids = map[string]string{}
	w.Fake.mu.Unlock()
	// As in production: the state directory exists (and is secured) before anything is written into it.
	if err := os.MkdirAll(w.State.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.Fake.mu.Lock()
	w.Fake.onUp = func() {
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{"router": "router-1", "index": "index-1"}
		w.Fake.mu.Unlock()
		w.setHealthy()
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

// skipIfRoot skips t when running as root, which bypasses the file
// permissions a symlink/mode-based test stages.
func skipIfRoot(t *testing.T) {
	t.Helper()
	// Root reads a 0000 file regardless, so there would be nothing to refuse or warn about.
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses the file permissions this test tampers with")
	}
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

// mustContain fails t unless want is one of lines, exactly.
func mustContain(t *testing.T, lines []string, want string) {
	t.Helper()
	if !slices.Contains(lines, want) {
		t.Fatalf("expected lines to contain %q; got %v", want, lines)
	}
}
