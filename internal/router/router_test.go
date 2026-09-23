package router_test

// Ported from tests/unit/router.test.ts. Test titles are kept verbatim so
// coverage can be compared title by title with the TypeScript suite. Every
// fixture and expectation uses router.Protocol/router.RoutedBySinceProtocol
// symbolically, so it is automatically consistent with WP3's protocol-4 bump
// (#39) without literal "3"s baked in anywhere the TypeScript source had them.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

const (
	testProject = "berth-router"
	testNetwork = "berth-router"
	testNS      = "dev.berth"
)

/* --------------------------------------------------------------- the fake */

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
	// psExitCode/psStderr is what `compose ps` says when Docker itself is the problem.
	psExitCode int
	psStderr   string
	// dockerMissing pretends `docker` is not installed at all.
	dockerMissing bool
	// onUp is called on every `up`, so a test can make the start itself bring containers back.
	onUp func()
	// onRecreate is called when `up --force-recreate` runs, so a test can make the recreation "fix" things.
	onRecreate func()
}

func newDockerFake() *dockerFake {
	return &dockerFake{
		ids:  map[string]string{"router": "router-1", "docker-api": "api-1", "index": "index-1"},
		docs: map[string]router.ContainerInspect{},
	}
}

func containsArg(args []string, want string) bool { return slices.Contains(args, want) }

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
		return run.Result{Stderr: d.downStderr, ExitCode: d.downExitCode}, nil
	case len(args) > 0 && args[0] == "compose":
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

func (d *dockerFake) Which(string) bool { return true }

func (d *dockerFake) snapshotCalls() []call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]call(nil), d.calls...)
}

func (d *dockerFake) setDoc(id string, doc router.ContainerInspect) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs[id] = doc
}

func (d *dockerFake) deleteDoc(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.docs, id)
}

func (d *dockerFake) clearDocs() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs = map[string]router.ContainerInspect{}
}

/* --------------------------------------------------------------- fixtures */

func testLabels(service string) map[string]string {
	return map[string]string{
		"com.docker.compose.project": testProject,
		"com.docker.compose.service": service,
		testNS + ".component":        service,
		testNS + ".router-protocol":  strconv.Itoa(router.Protocol),
	}
}

// routerDoc is what Docker reports for a router started exactly as the stack asks.
func routerDoc(dynamicDir string, port int) router.ContainerInspect {
	return router.ContainerInspect{
		Config: router.InspectConfig{
			Cmd:    router.RouterCommand(testNetwork),
			Image:  router.RouterImage,
			Labels: testLabels("router"),
		},
		HostConfig: router.InspectHostConfig{
			CapAdd: []string{"CAP_NET_BIND_SERVICE"}, CapDrop: []string{"ALL"},
			ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
		},
		Mounts: []router.InspectMount{
			{Type: "bind", Source: dynamicDir, Destination: router.DynamicConfigMount, RW: false},
		},
		NetworkSettings: router.InspectNetworkSettings{
			Networks: map[string]json.RawMessage{
				testNetwork:                 json.RawMessage("{}"),
				testProject + "_docker-api": json.RawMessage("{}"),
			},
			Ports: map[string][]router.InspectPortBinding{
				"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
			},
		},
		State: router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
}

func apiDoc() router.ContainerInspect {
	access := router.DockerAPIAccess()
	names := make([]string, 0, len(access))
	for name := range access {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+access[name])
	}
	return router.ContainerInspect{
		Config: router.InspectConfig{Env: env, Image: router.DockerAPIImage, Labels: testLabels("docker-api")},
		HostConfig: router.InspectHostConfig{
			CapAdd: []string{}, CapDrop: []string{"ALL"}, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
		},
		Mounts: []router.InspectMount{
			{Type: "bind", Source: router.DockerSocket, Destination: router.DockerSocket, RW: false},
		},
		NetworkSettings: router.InspectNetworkSettings{
			Networks: map[string]json.RawMessage{testProject + "_docker-api": json.RawMessage("{}")},
			Ports:    map[string][]router.InspectPortBinding{},
		},
		State: router.InspectState{Status: "running", Health: router.InspectHealth{Status: "healthy"}},
	}
}

func indexDoc(indexDir string) router.ContainerInspect {
	labels := testLabels("index")
	for key, value := range router.IndexRouteLabels(testNetwork).Map() {
		labels[key] = value
	}
	return router.ContainerInspect{
		Config: router.InspectConfig{Cmd: append([]string(nil), router.IndexCommand...), Image: router.IndexImage, Labels: labels},
		HostConfig: router.InspectHostConfig{
			CapAdd: []string{}, CapDrop: []string{"ALL"}, ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges:true"},
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

func newState(dir string) layout.RouterLayout {
	indexDir := filepath.Join(dir, "index")
	return layout.RouterLayout{
		StateDir:     dir,
		ComposeFile:  filepath.Join(dir, "compose.yaml"),
		ProtocolFile: filepath.Join(dir, "protocol"),
		DynamicDir:   filepath.Join(dir, "dynamic"),
		IndexDir:     indexDir,
		IndexFile:    filepath.Join(indexDir, "index.html"),
		LockPath:     filepath.Join(dir, "lock"),
	}
}

type testWorld struct {
	t      *testing.T
	Dir    string
	State  layout.RouterLayout
	Fake   *dockerFake
	Logger *logger.Recording
	Router *router.Router
	Port   int
}

func (w *testWorld) setHealthy() {
	w.Fake.setDoc("router-1", routerDoc(w.State.DynamicDir, w.Port))
	w.Fake.setDoc("api-1", apiDoc())
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

func newWorld(t *testing.T, port int, existing string) *testWorld {
	t.Helper()
	return newWorldOpts(t, existing, port, nil)
}

// taken is a control home whose router containers are gone — what `router
// down` leaves. protocol records one as last brought up when non-nil.
// Starting it again brings the containers back, as a real `up` would.
func taken(t *testing.T, w *testWorld, protocol *int) {
	t.Helper()
	w.Fake.mu.Lock()
	w.Fake.ids = map[string]string{}
	w.Fake.mu.Unlock()
	// As in production: the state directory exists (and is secured) before anything is written into it.
	if err := os.MkdirAll(w.State.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if protocol != nil {
		if err := os.WriteFile(w.State.ProtocolFile, []byte(strconv.Itoa(*protocol)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.Fake.mu.Lock()
	w.Fake.onUp = func() {
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{"router": "router-1", "docker-api": "api-1", "index": "index-1"}
		w.Fake.mu.Unlock()
		w.setHealthy()
	}
	w.Fake.mu.Unlock()
}

func protocolPtr(v int) *int { return &v }

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

func (w *testWorld) firstUp() call {
	for _, c := range w.Fake.snapshotCalls() {
		if len(c.args) > 0 && c.args[0] == "compose" && containsArg(c.args, "up") {
			return c
		}
	}
	return call{}
}

// unrouted is the exact line noteUnroutedAfterUpgrade emits, so asserting
// its absence cannot go vacuous.
const unrouted = "warn: Worktrees already up elsewhere stay unrouted until their next 'duke up'."

func skipIfRoot(t *testing.T) {
	t.Helper()
	// Root reads a 0000 file regardless, so there would be nothing to refuse or warn about.
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses the file permissions this test tampers with")
	}
}

func fileMode(t *testing.T, file string) os.FileMode {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat %s: %v", file, err)
	}
	return info.Mode().Perm()
}

func linesWithPrefix(lines []string, prefix string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

func containsSubstring(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, lines []string, want string) {
	t.Helper()
	if !slices.Contains(lines, want) {
		t.Fatalf("expected lines to contain %q; got %v", want, lines)
	}
}

func mustNotContain(t *testing.T, lines []string, want string) {
	t.Helper()
	if slices.Contains(lines, want) {
		t.Fatalf("expected lines not to contain %q; got %v", want, lines)
	}
}
