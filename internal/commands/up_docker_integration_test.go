//go:build integration

package commands_test

// `up` against a real daemon, the way a developer uses it: up, the routed
// host answers, then down and worktree remove — once from a fresh checkout,
// and once over a worktree the TypeScript build brought up first. The
// router is a real shared router, but one of this test's own name, port and
// state directory (commands.SharedRouter), so it runs beside a developer's
// real one without touching it.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run/osrun"
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

// walkToml is berth.toml for walkCompose: one service, on the index, with
// hooks that record every stage they run. %s takes the run's own label namespace.
const walkToml = `[project]
name = "walk"
domain = "walk.localhost"
label-namespace = %q
[compose]
file = "compose.yaml"
[[service]]
name = "api"
[service.index]
label = "API"
[hooks]
prepare = "./hooks/record"
seed = "./hooks/record"
`

// walk is one checkout and one test router on a real daemon.
type walk struct {
	base, checkout, controlHome, namespace, log string
	port                                        int
	// portFromSettings makes the getenv leave BERTH_ROUTER_PORT unset, so
	// the port must come from settings.toml under the berth root (#79).
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
// namespace) and compose written as the checkout's own berth.toml and
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
		namespace: "dev.berth-test-" + namePrefix + "-" + id, log: filepath.Join(base, "hooks.log"), port: freePort(t),
	}
	project := "berth-router-test-" + namePrefix + "-" + id
	t.Cleanup(func() {
		// The router project comes down first (found in review, round 2 of
		// #55): its router container may still be attached to a
		// per-worktree router network the label sweep below is about to
		// try to remove — attached the way ConnectNetwork attaches it,
		// which the router's own compose file never declares, so `down`
		// alone would not otherwise touch it. Removing the container here
		// disconnects it from every network it was attached to, including
		// that one, so the sweep's own network removal does not fail with
		// "has active endpoints" and leak the network.
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
		_ = exec.Command("docker", "network", "rm", project).Run()
	})

	if err := os.MkdirAll(w.checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.checkout, "berth.toml"), fmt.Sprintf(tomlBody, w.namespace), 0o644)
	writeFile(t, filepath.Join(w.checkout, "compose.yaml"), compose, 0o644)
	gitIn(t, w.checkout, "init", "--initial-branch="+branch, ".")
	gitIn(t, w.checkout, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")

	getenv := func(key string) (string, bool) {
		switch key {
		// BERTH_CONTROL_HOME, not BERTH_HOME: this world's fixtures (see
		// layout.WorktreeLayout(w.controlHome, ...) below) are written
		// directly at w.controlHome, so it must resolve as the control home
		// itself rather than have "control-home" derived underneath it
		// (#80). The router still resolves under HOME below, isolated from
		// this machine's real one; this walk brings up its own.
		case "BERTH_CONTROL_HOME":
			return w.controlHome, true
		case "HOME":
			return filepath.Join(base, "home"), true
		case "BERTH_ROUTER_PORT":
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
		CommandName: "berth", Invocation: "berth up", Logger: logger.Silent{}, Getenv: getenv,
		Environ: os.Environ(), Cwd: w.checkout, Runner: osrun.New(),
	}
	// Home is the berth root the getenv above resolves (HOME/.berth), so the
	// router this walk starts is labelled as this walk's own (#82) — what
	// doctor, resolving the same root, then reports as its owner.
	home, err := layout.ResolveHome(getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	w.svc, err = router.New(router.Options{
		State: state, Port: w.port, Runner: w.env.Runner, Logger: logger.Silent{},
		ProjectName: project, Network: project, LabelNamespace: w.namespace, Environ: os.Environ(),
		CommandName: "berth", Home: home,
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
	writeFile(t, filepath.Join(w.checkout, "hooks", "record"), fmt.Sprintf("#!/bin/sh\necho \"$BERTH_STAGE\" >> %q\n", w.log), 0o755)
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
	// down and worktree remove unroute through this, so they reach the
	// test's own router and never the real one's default project.
	cctx.SharedRouter = w.svc
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

	cctx := w.context(t)
	if _, err := commands.RemoveWithContext(ctx, cctx, w.env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}, {"image", "ls", "-q"}} {
		if left := w.owned(t, registration.WorktreeID, kind...); len(left) != 0 {
			t.Fatalf("%v left behind: %v", kind, left)
		}
	}
	paths := layout.WorktreeLayout(w.controlHome, registration.RepositoryID, registration.WorktreeID)
	for _, path := range []string{paths.Home, paths.RecordPath, filepath.Join(w.checkout, ".git", "berth", "worktree-id")} {
		if found, _ := files.PathExists(path); found {
			t.Fatalf("%s survived worktree remove", path)
		}
	}
}

func TestUpDownRemoveOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const host = "feature-walk.walk.localhost"

	result := w.up(t)

	if !slices.Equal(stageNames(result), []string{"prepare", "seed"}) || !slices.Equal(w.stages(t), []string{"prepare", "seed"}) {
		t.Fatalf("hooks = %v, log = %v", result.Hooks, w.stages(t))
	}
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	// The index `up` refreshed lists the worktree, running, through the same router.
	w.eventually(t, router.IndexHost, func(status int, body string) bool {
		return status == http.StatusOK && strings.Contains(body, fmt.Sprintf(`href="http://%s:%d"`, host, w.port)) &&
			strings.Contains(body, "status-running")
	})

	w.downAndRemove(t, result, host)
}

// TestUpUsesTheSettingsFilePortOnARealDaemon is #79 against a real daemon:
// with the router's port set only in settings.toml under the berth root,
// every command resolves that port — the context `up` builds, the URLs it
// prints and the index page it refreshes all carry it, and nothing falls
// back to 80. The walk's own router already listens on that port (it is
// this test's, never the machine's), so what this proves is the resolution
// from the file, not the environment.
func TestUpUsesTheSettingsFilePortOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	w.portFromSettings = true
	if _, set := w.env.Getenv("BERTH_ROUTER_PORT"); set {
		t.Fatal("BERTH_ROUTER_PORT must be unset for this test to prove anything")
	}
	home, _ := w.env.Getenv("HOME")
	root := filepath.Join(home, ".berth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "settings.toml"), fmt.Sprintf("[router]\nport = %d\n", w.port), 0o600)
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

func TestTypeScriptWritesGoUps(t *testing.T) {
	w := newWalk(t)
	nodeBin := node(t)
	const host = "feature-walk.walk.localhost"

	// Up, by the TypeScript build: registered, initialized, and running —
	// but never routed, since it starts no router.
	legacy := exec.Command(nodeBin, filepath.Join(repoRoot(t), "tools", "oracle", "legacy-up.ts"), w.controlHome, w.checkout)
	legacy.Dir = repoRoot(t)
	legacy.Env = append(os.Environ(), "BERTH_HOME="+w.controlHome)
	output, err := legacy.Output()
	if err != nil {
		var stderr []byte
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = exit.Stderr
		}
		t.Fatalf("legacy up: %v\n%s", err, stderr)
	}
	var registered struct{ RepositoryID, WorktreeID, Slug string }
	if err := json.Unmarshal(output, &registered); err != nil {
		t.Fatalf("legacy up said %q: %v", output, err)
	}
	if legacy, err := files.LegacyControlHome(w.controlHome); err != nil || !legacy {
		t.Fatalf("the TypeScript build's control home is not recognised as its: %v, %v", legacy, err)
	}

	// Up, by the Go build, over it.
	result := w.up(t)

	if result.Registration.RepositoryID != registered.RepositoryID || result.Registration.WorktreeID != registered.WorktreeID ||
		result.Registration.Slug != registered.Slug {
		t.Fatalf("registration = %+v, want the TypeScript build's %+v", result.Registration, registered)
	}
	// The TypeScript build completed initialization, so seed never runs again.
	if !slices.Equal(stageNames(result), []string{"prepare"}) || !slices.Equal(w.stages(t), []string{"prepare"}) {
		t.Fatalf("hooks = %v, log = %v", result.Hooks, w.stages(t))
	}
	if running := w.owned(t, registered.WorktreeID, "ps", "-q"); len(running) != 1 {
		t.Fatalf("running = %v, want the one api container", running)
	}
	marker, err := os.ReadFile(filepath.Join(w.controlHome, files.ControlHomeMarker))
	if err != nil || string(marker) != files.ControlHomeFormat {
		t.Fatalf("control home marker = %q, %v; want the Go build's format written", marker, err)
	}
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })

	w.downAndRemove(t, result, host)
}
