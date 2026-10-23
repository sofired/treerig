//go:build integration

package commands_test

// `up` against a real daemon, the way a developer uses it: up, the routed
// host answers, then down and worktree remove — once from a fresh checkout,
// and once run a second time over the same, already-registered and
// initialized worktree. The router is a real shared router, but one of
// this test's own name, port and state directory (commands.SharedRouter),
// so it runs beside a developer's real one without touching it.

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run/osrun"
)

const walkCompose = `services:
  api:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from api' > /www/index.html && exec httpd -f -p 8080 -h /www"]
    expose: ["8080"]
    volumes: ["data:/data"]
volumes:
  data: {}
`

// walkToml is treerig.toml for walkCompose: one service, on the index, with
// hooks that record every stage they run. %s takes the run's own label namespace.
const walkToml = `[project]
name = "walk"
domain = "walk.localhost"
label-namespace = %q
[compose]
file = "compose.yaml"
[service.api]
[service.api.index]
label = "API"
[hooks]
prepare = "./hooks/record"
seed = "./hooks/record"
`

// walkRouterRange is where the test routers of this package allocate their
// checkout-index and ping ports from. It is inside the test range but apart
// from the router package's (30900-30999) and from the whole-range picks the
// binary-driven tests make against a fake Docker: the picks are deterministic,
// so suites running at the same moment would otherwise race for the same ports.
var walkRouterRange = layout.PortRange{Low: 30850, High: 30899}

// walk is one checkout and one test router on a real daemon.
type walk struct {
	base, checkout, controlHome, namespace, log string
	port                                        int
	// portFromSettings makes the getenv leave TREERIG_ROUTER_PORT unset, so
	// the port must come from settings.toml under the treerig root (#79).
	portFromSettings bool
	env              *commands.Env
	svc              *router.Router
	state            layout.RouterLayout
}

// randomSuffix is a short random hex string, for names unique to one test run.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

// newWalkFixture is the recipe behind every checkout-and-test-router fixture
// on a real daemon: a fresh control home, a randomised label namespace and
// project name under namePrefix so it runs beside a developer's real router
// and worktrees without colliding, tomlBody (its own %q takes the
// namespace) and compose written as the checkout's own treerig.toml and
// compose.yaml, a Git repository on branch, and a cleanup that removes
// everything the test's namespace ends up owning. Callers that need more
// than the checkout itself — newWalk's own hooks script — add it to the
// returned walk before using it.
func newWalkFixture(t *testing.T, namePrefix, branch, tomlBody, compose string) *walk {
	t.Helper()
	dockerAvailable(t)
	id := randomSuffix(t)
	base := registrytest.TempDir(t)
	w := &walk{
		base: base, checkout: filepath.Join(base, "checkout"), controlHome: filepath.Join(base, "control"),
		namespace: "dev.treerig-test-" + namePrefix + "-" + id, log: filepath.Join(base, "hooks.log"), port: freePort(t),
	}
	project := "treerig-router-test-" + namePrefix + "-" + id
	t.Cleanup(func() {
		// The router project comes down first, so nothing of it is left
		// when the label sweep below runs.
		if err := exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run(); err != nil {
			// A failed down can leave the router attached. Its containers
			// don't carry this test's managed label, so remove them by the
			// Compose project label, which only this test's router uses.
			for _, containerID := range dockerIDs(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+project) {
				_ = exec.Command("docker", "rm", "--force", containerID).Run()
			}
		}
		// Whatever the test left, by the label only this test's resources carry.
		owned := "label=" + brand.ManagedLabel(w.namespace) + "=true"
		for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}} {
			for _, resourceID := range dockerIDs(t, append(kind, "--filter", owned)...) {
				if kind[0] == "ps" {
					_ = exec.Command("docker", "rm", "--force", resourceID).Run()
				} else {
					_ = exec.Command("docker", kind[0], "rm", resourceID).Run()
				}
			}
		}
		_ = exec.Command("docker", "network", "rm", project+"_default").Run()
	})

	if err := os.MkdirAll(w.checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.checkout, "treerig.toml"), fmt.Sprintf(tomlBody, w.namespace), 0o644)
	writeFile(t, filepath.Join(w.checkout, "compose.yaml"), compose, 0o644)
	gitIn(t, w.checkout, "init", "--initial-branch="+branch, ".")
	gitIn(t, w.checkout, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")

	getenv := func(key string) (string, bool) {
		switch key {
		// TREERIG_CONTROL_HOME, not TREERIG_HOME: this world's fixtures (see
		// layout.WorktreeLayout(w.controlHome, ...) below) are written
		// directly at w.controlHome, so it must resolve as the control home
		// itself rather than have "control-home" derived underneath it
		// (#80). The router still resolves under HOME below, isolated from
		// this machine's real one; this walk brings up its own.
		case "TREERIG_CONTROL_HOME":
			return w.controlHome, true
		case "HOME":
			return filepath.Join(base, "home"), true
		case "TREERIG_ROUTER_PORT":
			if w.portFromSettings {
				return "", false
			}
			return strconv.Itoa(w.port), true
		}
		return "", false
	}
	registrytest.RequireIsolatedRouterState(t, getenv)
	state, err := layout.RouterState(getenv)
	if err != nil {
		t.Fatal(err)
	}
	w.state = state
	w.env = &commands.Env{
		CommandName: "treerig", Invocation: "treerig up", Logger: logger.Silent{}, Getenv: getenv,
		Environ: os.Environ(), Cwd: w.checkout, Runner: osrun.New(), OS: runtime.GOOS,
	}
	// Home is the treerig root the getenv above resolves (HOME/.treerig), so the
	// router this walk starts is labelled as this walk's own (#82) — what
	// doctor, resolving the same root, then reports as its owner.
	home, err := layout.ResolveHome(getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	// A scratch treerig root takes its loopback ports from the test range, never
	// the default one a developer's own worktrees use (#119).
	registrytest.WriteScratchSettings(t, home, "")
	w.svc, err = router.New(router.Options{
		State: state, Port: w.port, Runner: w.env.Runner, Logger: logger.Silent{},
		ProjectName: project, LabelNamespace: w.namespace, Environ: os.Environ(),
		CommandName: "treerig", Home: home, ControlHome: w.controlHome,
		Registrations: commands.Registrations(w.controlHome), SharedRegistrations: commands.SharedRegistrations(w.controlHome), PortRange: walkRouterRange,
		ClientOS: runtime.GOOS,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// newWalk sets up one checkout and its own test router, over walkCompose's
// single service, plus the hooks script walkToml's [hooks] table points at.
func newWalk(t *testing.T) *walk {
	t.Helper()
	w := newWalkFixture(t, "up", "feature/walk", walkToml, walkCompose)
	if err := os.MkdirAll(filepath.Join(w.checkout, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.checkout, "hooks", "record"), fmt.Sprintf("#!/bin/sh\necho \"$TREERIG_STAGE\" >> %q\n", w.log), 0o755)
	return w
}

// freePort asks the kernel for an unused TCP port, for the test router to
// bind so it doesn't collide with a real one already running.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// context is one invocation's CommandContext.
func (w *walk) context(t *testing.T) *commands.CommandContext {
	t.Helper()
	cctx, err := commands.Context(w.env)
	if err != nil {
		t.Fatal(err)
	}
	return cctx
}

// up runs `up` against the walk's checkout and test router, failing the
// test on error.
func (w *walk) up(t *testing.T) *commands.UpResult {
	t.Helper()
	cctx := w.context(t)
	result, err := commands.UpWithContext(context.Background(), cctx,
		commands.SharedRouter(w.env, w.svc, w.state), commands.BestEffortIndex(cctx), commands.UpOptions{})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	return result
}

// walkClient gives up on one request quickly: a route Traefik has not
// dropped yet, just after `down`, hangs until Traefik's own 30 s gateway
// timeout, and eventually should ask again rather than wait that out.
var walkClient = &http.Client{Timeout: 2 * time.Second}

// get is a GET through the test router, by hostname.
func (w *walk) get(host string) (int, string) {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", w.port), nil)
	if err != nil {
		return 0, err.Error()
	}
	request.Host = host
	response, err := walkClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// eventually waits for the router to answer host the way accept wants:
// Traefik learns about containers from Docker's events, a moment later.
func (w *walk) eventually(t *testing.T, host string, accept func(int, string) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body := w.get(host)
		if accept(status, body) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s answered %d %q", host, status, body)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stages returns the hook stages recorded in the walk's log file, in the
// order the hooks ran.
func (w *walk) stages(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(w.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// owned is every id of kind carrying worktreeID's ownership label.
func (w *walk) owned(t *testing.T, worktreeID string, kind ...string) []string {
	t.Helper()
	return dockerIDs(t, append(kind, "--filter", "label="+brand.WorktreeLabel(w.namespace)+"="+worktreeID)...)
}

// downAndRemove is the rest of the walk: down stops the services, the host
// stops being routed, and worktree remove leaves nothing of the worktree
// behind, by its labels or on disk.
func (w *walk) downAndRemove(t *testing.T, result *commands.UpResult, host string) {
	t.Helper()
	ctx := context.Background()
	registration := result.Registration
	if err := commands.DownWithContext(ctx, w.context(t)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if running := w.owned(t, registration.WorktreeID, "ps", "-q"); len(running) != 0 {
		t.Fatalf("still running after down: %v", running)
	}
	w.eventually(t, host, func(status int, _ string) bool { return status == http.StatusNotFound })
	w.requireNoRouteFiles(t, registration.WorktreeID)

	cctx := w.context(t)
	if _, err := commands.RemoveWithContext(ctx, cctx, w.env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}, {"image", "ls", "-q"}} {
		if left := w.owned(t, registration.WorktreeID, kind...); len(left) != 0 {
			t.Fatalf("%v left behind: %v", kind, left)
		}
	}
	w.requireNoRouteFiles(t, registration.WorktreeID)
	paths := layout.WorktreeLayout(w.controlHome, registration.RepositoryID, registration.WorktreeID)
	for _, path := range []string{paths.Home, paths.RecordPath, filepath.Join(w.checkout, ".git", "treerig", "worktree-id")} {
		if found, _ := files.PathExists(path); found {
			t.Fatalf("%s survived worktree remove", path)
		}
	}
}

// routeFiles are the route files the walk's router holds for worktreeID.
func (w *walk) routeFiles(t *testing.T, worktreeID string) []router.FragmentInfo {
	t.Helper()
	fragments, err := router.ListFragments(w.state)
	if err != nil {
		t.Fatal(err)
	}
	var mine []router.FragmentInfo
	for _, fragment := range fragments {
		if fragment.WorktreeID == worktreeID {
			mine = append(mine, fragment)
		}
	}
	return mine
}

// requireNoRouteFiles fails the test while any route file of worktreeID exists.
func (w *walk) requireNoRouteFiles(t *testing.T, worktreeID string) {
	t.Helper()
	if left := w.routeFiles(t, worktreeID); len(left) != 0 {
		t.Fatalf("route files left behind: %+v", left)
	}
}

// requireNoRoutingNetwork fails the test when the worktree owns a network
// besides Compose's default one: nothing routes through a network of its own.
func (w *walk) requireNoRoutingNetwork(t *testing.T, worktreeID string) {
	t.Helper()
	for _, name := range dockerNames(t, "network", "ls", "--filter", "label="+brand.WorktreeLabel(w.namespace)+"="+worktreeID, "--format", "{{.Name}}") {
		if !strings.HasSuffix(name, "_default") {
			t.Fatalf("the worktree owns the network %s; nothing should route through a network of its own", name)
		}
	}
}

// dockerNames runs docker with args and returns its non-empty output lines.
func dockerNames(t *testing.T, args ...string) []string {
	t.Helper()
	output, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.Fields(string(output))
}

func TestUpDownRemoveOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const host = "feature-walk.walk.localhost"

	result := w.up(t)

	if !slices.Equal(stageNames(result), []string{"prepare", "seed"}) || !slices.Equal(w.stages(t), []string{"prepare", "seed"}) {
		t.Fatalf("hooks = %v, log = %v", result.Hooks, w.stages(t))
	}
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	// The route file names the service's pinned loopback port, and no
	// network of the worktree's carries a route.
	routes := w.routeFiles(t, result.Registration.WorktreeID)
	port := w.recordedPorts(t, result)["api"]
	if len(routes) != 1 {
		t.Fatalf("route files = %+v", routes)
	}
	doc, err := router.ReadFragment(w.state, routes[0].Path)
	if err != nil || doc.TargetPort() != port {
		t.Fatalf("route file targets port %d (%v), want %d: %+v", doc.TargetPort(), err, port, doc)
	}
	for _, service := range doc.HTTP.Services {
		if got := service.LoadBalancer.Servers[0].URL; got != router.TargetURL(port) || !strings.HasPrefix(got, "http://"+router.LoopbackAlias+":") {
			t.Fatalf("route file target = %q, want %q", got, router.TargetURL(port))
		}
	}
	w.requireNoRoutingNetwork(t, result.Registration.WorktreeID)
	// The index `up` refreshed lists the worktree, running, through the same router.
	w.eventually(t, router.IndexHost, func(status int, body string) bool {
		return status == http.StatusOK && strings.Contains(body, fmt.Sprintf(`href="http://%s:%d"`, host, w.port)) &&
			strings.Contains(body, "status-running")
	})

	w.downAndRemove(t, result, host)
}

// TestRemoveFromOutsideARepositoryOnARealDaemon is #200 against a real daemon:
// a worktree that is up is removed by its slug from a directory that is in no
// Git repository and has no treerig.toml above it, under the namespace its
// registration records, and leaves nothing of itself in Docker, the router or
// the control home. Nothing is warned about Git or the missing file, and the
// index refresh that follows does not mistake the directory for a repository.
func TestRemoveFromOutsideARepositoryOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	result := w.up(t)
	registration := result.Registration
	if len(w.owned(t, registration.WorktreeID, "ps", "-q")) == 0 {
		t.Fatal("nothing is running after up")
	}

	elsewhere := filepath.Join(w.base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	log := &logger.Recording{}
	outside := *w.env
	outside.Cwd, outside.Logger = elsewhere, log
	cctx, err := commands.ContextWithoutConfig(&outside)
	if err != nil {
		t.Fatal(err)
	}
	if cctx.Loaded != nil {
		t.Fatalf("a treerig.toml was loaded from %s", elsewhere)
	}

	removed, err := commands.RemoveWithContext(context.Background(), cctx, &outside, commands.RemoveOptions{Identifier: "feature-walk", Yes: true})
	if err != nil {
		t.Fatalf("worktree remove from outside: %v", err)
	}
	if !removed.Registration || !removed.Home {
		t.Fatalf("result = %+v", removed)
	}
	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}, {"image", "ls", "-q"}} {
		if left := w.owned(t, registration.WorktreeID, kind...); len(left) != 0 {
			t.Fatalf("%v left behind: %v", kind, left)
		}
	}
	w.requireNoRouteFiles(t, registration.WorktreeID)
	paths := layout.WorktreeLayout(w.controlHome, registration.RepositoryID, registration.WorktreeID)
	for _, path := range []string{paths.Home, paths.RecordPath} {
		if found, _ := files.PathExists(path); found {
			t.Fatalf("%s survived worktree remove", path)
		}
	}
	// The directory is not this command's to touch: its identity marker stays
	// until a removal that runs from the repository (or the later removal of
	// the directory itself) takes it.
	// TODO(#211): removal from outside the repository removes the marker too.
	marker := filepath.Join(w.checkout, ".git", "treerig", "worktree-id")
	if found, _ := files.PathExists(marker); !found {
		t.Errorf("%s was removed from outside the repository", marker)
	}
	// Nothing here is a reason to warn: not Git's refusal for the index
	// refresh that follows, not a domain the missing treerig.toml would have
	// said. No warning is expected at all.
	lines, _ := log.Snapshot()
	for _, line := range lines {
		if strings.HasPrefix(line, "warn:") {
			t.Errorf("unexpected warning: %q", line)
		}
	}
}

// TestUpUsesTheSettingsFilePortOnARealDaemon is #79 against a real daemon:
// with the router's port set only in settings.toml under the treerig root,
// every command resolves that port — the context `up` builds, the URLs it
// prints and the index page it refreshes all carry it, and nothing falls
// back to 80. The walk's own router already listens on that port (it is
// this test's, never the machine's), so what this proves is the resolution
// from the file, not the environment.
func TestUpUsesTheSettingsFilePortOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	w.portFromSettings = true
	if _, set := w.env.Getenv("TREERIG_ROUTER_PORT"); set {
		t.Fatal("TREERIG_ROUTER_PORT must be unset for this test to prove anything")
	}
	home, _ := w.env.Getenv("HOME")
	root := filepath.Join(home, ".treerig")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	registrytest.WriteScratchSettings(t, root, fmt.Sprintf("[router]\nport = %d\n", w.port))
	const host = "feature-walk.walk.localhost"

	if cctx := w.context(t); cctx.RouterPort != w.port {
		t.Fatalf("RouterPort = %d, want %d from settings.toml", cctx.RouterPort, w.port)
	}
	result := w.up(t)
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	w.eventually(t, router.IndexHost, func(status int, body string) bool {
		return status == http.StatusOK && strings.Contains(body, fmt.Sprintf(`href="http://%s:%d"`, host, w.port))
	})

	w.downAndRemove(t, result, host)
}

// TestUpOverAnAlreadyRegisteredWorktreeOnARealDaemon proves that `up` run a
// second time over the same worktree — already registered, initialized, and
// running — adopts the same registration and reruns only the prepare hook,
// not seed, against a real daemon.
func TestUpOverAnAlreadyRegisteredWorktreeOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const host = "feature-walk.walk.localhost"

	first := w.up(t)
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })

	second := w.up(t)
	if second.Registration.RepositoryID != first.Registration.RepositoryID || second.Registration.WorktreeID != first.Registration.WorktreeID ||
		second.Registration.Slug != first.Registration.Slug || second.Registration.Domain != first.Registration.Domain {
		t.Fatalf("registration = %+v, want the same %+v", second.Registration, first.Registration)
	}
	if !slices.Equal(stageNames(second), []string{"prepare"}) || !slices.Equal(w.stages(t), []string{"prepare", "seed", "prepare"}) {
		t.Fatalf("hooks = %v, log = %v", second.Hooks, w.stages(t))
	}
	if running := w.owned(t, second.Registration.WorktreeID, "ps", "-q"); len(running) != 1 {
		t.Fatalf("running = %v, want the one api container", running)
	}
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })

	w.downAndRemove(t, second, host)
}

// TestRouterEnsureRepairsStaleRouteFilesOnARealDaemon is decision 9 against a
// real daemon: a route file whose registration records another port is
// pointed at the recorded one by the router's next Ensure, and a file this
// control home holds no record of is left in place and reported by doctor
// (instance "unmatched").
func TestRouterEnsureRepairsStaleRouteFilesOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const host = "feature-walk.walk.localhost"
	result := w.up(t)
	registration := result.Registration
	port := w.recordedPorts(t, result)["api"]
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })

	// A file for a worktree of the same repository that this control home holds
	// no record of: reported by doctor, never removed (it may be another control
	// home's).
	vanished := "55555555-5555-4555-8555-555555555555"
	doc := router.NewFragmentDocument("vanished-api", "Host(`vanished.walk.localhost`)", 30998)
	if err := router.WriteFragment(w.state, registration.RepositoryID, vanished, "api", doc); err != nil {
		t.Fatal(err)
	}
	// A live worktree's file pointing at a port its record does not hold.
	stale := router.NewFragmentDocument("wrong-port", fmt.Sprintf("Host(`%s`)", host), port+1)
	if err := router.WriteFragment(w.state, registration.RepositoryID, registration.WorktreeID, "api", stale); err != nil {
		t.Fatal(err)
	}

	if err := commands.SharedRouter(w.env, w.svc, w.state).Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if left := w.routeFiles(t, vanished); len(left) != 1 {
		t.Fatalf("the unmatched worktree's route file = %+v, want it left alone", left)
	}
	t.Cleanup(func() { _ = router.RemoveWorktreeFragments(w.state, registration.RepositoryID, vanished) })
	sections, err := commands.DoctorSections(context.Background(), w.env, w.svc, w.state)
	if err != nil {
		t.Fatalf("DoctorSections: %v", err)
	}
	reported := false
	for _, check := range routerChecks(sections) {
		reported = reported || (check.ID == "router.orphaned-routes" && check.Instance == "unmatched")
	}
	if !reported {
		t.Fatalf("doctor did not report the unmatched route file: %+v", routerChecks(sections))
	}
	routes := w.routeFiles(t, registration.WorktreeID)
	if len(routes) != 1 {
		t.Fatalf("route files = %+v", routes)
	}
	repaired, err := router.ReadFragment(w.state, routes[0].Path)
	if err != nil || repaired.TargetPort() != port {
		t.Fatalf("route file targets %d (%v), want the recorded %d", repaired.TargetPort(), err, port)
	}
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })

	w.downAndRemove(t, result, host)
}
