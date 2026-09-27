//go:build integration

package router_test

// Real-daemon tests of the shared router: sharing across control homes
// (#12, #13), lock contention and timeouts (#14), routing, and doctor's view
// of it. Every fixture uses a unique project, network, port and state
// directory and cleans up after itself, so it can run beside a developer's
// real router and beside another Docker suite on the same daemon. Tests
// that need a routed worktree live with `up` in
// internal/commands/up_routes_docker_integration_test.go.

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/osrun"
)

// freePortForTest asks the kernel for a free loopback port.
func freePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// requireDocker skips t (or fails it, under BERTH_TEST_REQUIRE_DOCKER=1)
// when no Docker daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("BERTH_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (BERTH_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

// suffix is a short random hex string, for names unique to one test run.
func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

// dockerOutput runs a real `docker` command, failing t if it exits non-zero.
func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fetchThroughRouter is a GET to 127.0.0.1:port with an explicit Host
// header, the way a request actually reaches the router by hostname. Returns
// the status and body, or 0 and the error's own message when the request
// itself failed (a connection refusal, say).
func fetchThroughRouter(t *testing.T, port int, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// eventually retries probe (Traefik learns about containers from events, and
// needs a moment) until accept is satisfied or timeout elapses.
func eventually[T any](t *testing.T, timeout time.Duration, probe func() T, accept func(T) bool) T {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last T
	for {
		last = probe()
		if accept(last) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s; last value: %+v", timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestSharedRouterOnARealDaemon is the shared router against a real daemon:
// it starts hardened, routes the machine-wide index by hostname, and its
// socket proxy refuses everything a router does not need.
func TestSharedRouterOnARealDaemon(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-" + id
	ns := "dev.berth-test-router-" + id
	dir := registrytest.TempDir(t)
	port := freePortForTest(t)
	state := newState(dir)

	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
		exec.Command("docker", "network", "rm", project).Run()
	})

	svc, err := router.New(router.Options{
		State: state, Port: port, Runner: osrun.New(), Logger: logger.Silent{},
		ProjectName: project, Network: project, LabelNamespace: ns, Environ: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("starts hardened, verifies clean, and is left alone by a second ensure", func(t *testing.T) {
		if err := svc.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
		problems, err := svc.Verify(ctx)
		if err != nil || len(problems) != 0 {
			t.Fatalf("problems = %v, err = %v", problems, err)
		}
		before := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")

		if err := svc.Ensure(ctx); err != nil {
			t.Fatal(err)
		}

		after := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
		if before != after {
			t.Fatalf("expected the same router container, got %q then %q", before, after)
		}
		status, err := svc.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running || len(status.Problems) != 0 || status.Port != port || status.Network != project {
			t.Fatalf("status = %+v", status)
		}
		logs := dockerOutput(t, "logs", before)
		if strings.Contains(logs, "level=error") {
			t.Errorf("expected no errors in the router's logs, got:\n%s", logs)
		}

		// The index answers through the router before anything has been indexed.
		body := eventually(t, 20*time.Second,
			func() string { _, b := fetchThroughRouter(t, port, router.IndexHost, "/"); return b },
			func(b string) bool { return strings.Contains(b, "No checkouts have been indexed yet") },
		)
		if !strings.Contains(body, "No checkouts have been indexed yet") {
			t.Errorf("index body = %q", body)
		}
	})

	// #55: the docker-socket-proxy is gone outright, and so is Traefik's
	// Docker-provider discovery — the router container has no access to the
	// Docker socket at all any more, and there is no docker-api service or
	// network left to check permissions against.
	t.Run("has no access to the Docker socket at all: no docker-api service, no socket mount", func(t *testing.T) {
		services := dockerOutput(t, "compose", "-p", project, "ps", "-a", "--format", "{{.Service}}")
		if strings.Contains(services, "docker-api") {
			t.Fatalf("expected no docker-api service, got %q", services)
		}
		if err := exec.Command("docker", "network", "inspect", project+"_docker-api").Run(); err == nil {
			t.Fatal("expected no docker-api network to exist")
		}
		routerID := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
		mounts := dockerOutput(t, "inspect", routerID, "--format", "{{range .Mounts}}{{.Source}} {{end}}")
		if strings.Contains(mounts, "docker.sock") {
			t.Fatalf("expected the router to have no Docker-socket mount, got %q", mounts)
		}
		cmd := dockerOutput(t, "inspect", routerID, "--format", "{{join .Config.Cmd \" \"}}")
		if strings.Contains(cmd, "--providers.docker") {
			t.Fatalf("expected no Docker-provider flag on the router's command line, got %q", cmd)
		}
	})

	// The rest of "routes a worktree by hostname..." (above), adapted to
	// #55: Traefik's Docker-provider discovery is gone, so
	// a container's own Traefik labels — everything a routed worktree
	// service used to need — do nothing any more; only a
	// dynamic-configuration fragment routes anything (fragment.go). The
	// control proves the positive, routed by a real fragment rather than
	// any label of its own; the intruder, carrying a complete-looking set
	// of Traefik labels but no fragment, proves the negative. Both share the
	// router's own network directly (no worktree, no Compose override, so
	// no per-worktree network of its own here) so the only difference
	// between them is whether a fragment exists.
	t.Run("never routes a container with Traefik labels but no fragment, and gives an unmatched host the router's own 404", func(t *testing.T) {
		intruderName := "intruder-" + id
		t.Cleanup(func() { exec.Command("docker", "rm", "--force", intruderName).Run() })
		dockerOutput(t, "run", "--detach", "--name", intruderName, "--network", project,
			"--label", "traefik.enable=true",
			"--label", "traefik.http.routers.intruder.rule=Host(`intruder.invalid.localhost`)",
			"--label", "traefik.http.routers.intruder.entrypoints="+router.RouterEntrypoint,
			"--label", "traefik.http.services.intruder.loadbalancer.server.port=8080",
			"busybox:1.37.0", "sh", "-c", "echo intruder > /tmp/index.html && exec httpd -f -p 8080 -h /tmp")

		// A control routed by a real fragment, not by any label of its own —
		// the only thing that routes anything now. Waited for with
		// eventually: the file provider's own watch takes a moment to pick
		// the fragment up, so once its route answers the router has
		// actually read it, and the intruder's 404 below is a verdict, not
		// a race with that watch.
		controlName := "control-" + id
		t.Cleanup(func() { exec.Command("docker", "rm", "--force", controlName).Run() })
		dockerOutput(t, "run", "--detach", "--name", controlName, "--network", project,
			"busybox:1.37.0", "sh", "-c", "echo control > /tmp/index.html && exec httpd -f -p 8080 -h /tmp")
		repositoryID, worktreeID := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
		t.Cleanup(func() { router.RemoveWorktreeFragments(state, repositoryID, worktreeID) })
		doc := router.NewFragmentDocument("control", "Host(`control.invalid.localhost`)", controlName, 8080)
		if err := router.WriteFragment(state, repositoryID, worktreeID, "control", doc); err != nil {
			t.Fatal(err)
		}
		controlBody := eventually(t, 20*time.Second,
			func() string { _, b := fetchThroughRouter(t, port, "control.invalid.localhost", "/"); return b },
			func(b string) bool { return strings.TrimSpace(b) == "control" },
		)
		if strings.TrimSpace(controlBody) != "control" {
			t.Fatalf("control host never routed; answered %q", controlBody)
		}

		nobodyStatus, nobodyBody := fetchThroughRouter(t, port, "nobody.invalid.localhost", "/")
		if nobodyStatus != http.StatusNotFound {
			t.Fatalf("unmatched host answered %d %q", nobodyStatus, nobodyBody)
		}
		// The negative means something only if the intruder is alive and serving.
		alive, err := exec.Command("docker", "exec", intruderName, "wget", "-qO-", "http://127.0.0.1:8080/").Output()
		if err != nil {
			t.Fatalf("intruder did not answer on its own: %v", err)
		}
		if strings.TrimSpace(string(alive)) != "intruder" {
			t.Fatalf("intruder served %q", alive)
		}
		// The router's own 404, byte for byte the one an unknown host gets — never the intruder's page, whatever labels it carries.
		intruderStatus, intruderBody := fetchThroughRouter(t, port, "intruder.invalid.localhost", "/")
		if intruderStatus != nobodyStatus || intruderBody != nobodyBody {
			t.Fatalf("intruder host answered %d %q, want the unmatched host's %d %q", intruderStatus, intruderBody, nobodyStatus, nobodyBody)
		}
		if strings.Contains(nobodyBody, "intruder") {
			t.Fatalf("unmatched host's body leaked the intruder's own: %q", nobodyBody)
		}
	})

	// A plain container joins the shared network directly, standing in for a
	// worktree's service: the network must stay while something is attached
	// and go once nothing is. Runs last: it stops the router itself.
	t.Run("keeps the shared network on down while something is attached, and lets it go once free", func(t *testing.T) {
		attachedName := "berth-router-test-attached-" + id
		// Registered by name before creation, so a failure partway through
		// `docker run` (the container created but not started, say) still
		// gets cleaned up — not only a successfully returned container id.
		t.Cleanup(func() { exec.Command("docker", "rm", "--force", attachedName).Run() })
		attached := dockerOutput(t, "run", "--detach", "--name", attachedName,
			"--network", project, "busybox:1.37.0", "sleep", "300")

		if err := svc.Down(ctx); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("docker", "network", "inspect", project).Run(); err != nil {
			t.Fatal("expected the network to still exist while something is attached")
		}

		if err := exec.Command("docker", "rm", "--force", attached).Run(); err != nil {
			t.Fatal(err)
		}
		if err := svc.Down(ctx); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("docker", "network", "inspect", project).Run(); err == nil {
			t.Fatal("expected the network to be gone once nothing is attached")
		}
	})
}

// TestTwoControlHomesShareOneRouter is #12/#13/#80: the router's state lives
// under the berth root (BERTH_HOME), outside every control home, so two
// control homes under the same berth root — the common case, since
// BERTH_CONTROL_HOME is an override, not the default — resolve to the same
// state directory and share one router; the second `ensure` from what is,
// in production, a different control home's process must not recreate
// anything.
func TestTwoControlHomesShareOneRouter(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-two-" + id
	ns := "dev.berth-test-router-two-" + id
	// One state directory — what layout.RouterState resolves to for one
	// berth root regardless of which control home under it is asking.
	dir := registrytest.TempDir(t)
	port := freePortForTest(t)
	state := newState(dir)

	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
		exec.Command("docker", "network", "rm", project).Run()
	})

	newHome := func(t *testing.T) *router.Router {
		svc, err := router.New(router.Options{
			State: state, Port: port, Runner: osrun.New(), Logger: logger.Silent{},
			ProjectName: project, Network: project, LabelNamespace: ns, Environ: os.Environ(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	ctx := context.Background()
	first := newHome(t)
	if err := first.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	before := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")

	// A second control home's own process, its own *router.Router, same state.
	second := newHome(t)
	if err := second.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	after := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
	if before != after {
		t.Fatalf("expected the second control home's ensure to find the same router, got %q then %q", before, after)
	}
}

// TestTwoBerthRootsRouterUpRefuses is #82: unlike two control homes under
// the same berth root (TestTwoControlHomesShareOneRouter, above), two
// different berth roots each resolve their own, separate router state —
// their own lock, their own compose.yaml, their own dynamic/index
// directories — but the router container itself is still one per Docker
// daemon (its Compose project name is fixed regardless of berth root). Before
// this row a second berth root's `router up` had no way to tell the running
// container was not its own, and force-recreated it out from under the
// first with no warning; this proves the fix end to end, on a real daemon:
// the second berth root's Ensure refuses, naming the first, and leaves the
// first's own running container exactly as it was — then proves the
// explicit way out ('router down' from either root, followed by the new
// root's own 'router up') hands it over cleanly.
func TestTwoBerthRootsRouterUpRefuses(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-roots-" + id
	ns := "dev.berth-test-router-roots-" + id
	port := freePortForTest(t)
	// Two separate berth roots: their own state directories (never shared,
	// unlike the two-control-homes case), their own Home labels.
	home1 := registrytest.TempDir(t)
	home2 := registrytest.TempDir(t)
	state1 := newState(filepath.Join(home1, "router"))
	state2 := newState(filepath.Join(home2, "router"))

	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
		exec.Command("docker", "network", "rm", project).Run()
	})

	newRoot := func(t *testing.T, state layout.RouterLayout, home string) *router.Router {
		svc, err := router.New(router.Options{
			State: state, Port: port, Runner: osrun.New(), Logger: logger.Silent{},
			ProjectName: project, Network: project, LabelNamespace: ns, Environ: os.Environ(),
			Home: home,
		})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	ctx := context.Background()
	first := newRoot(t, state1, home1)
	if err := first.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	before := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")

	second := newRoot(t, state2, home2)
	err := second.Ensure(ctx)
	want := fmt.Sprintf("The shared router belongs to a different berth root (%s).", home1)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want a refusal containing %q", err, want)
	}
	after := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
	if before != after {
		t.Fatalf("expected the second berth root's refusal to leave the first's router untouched, got %q then %q", before, after)
	}
	label := dockerOutput(t, "inspect", after, "--format", "{{index .Config.Labels \""+router.RouterHomeLabel+"\"}}")
	if label != home1 {
		t.Fatalf("router-home label = %q, want %q (the second root's refusal must not have touched it)", label, home1)
	}

	// The explicit way out (#82): 'router down' stops it whoever owns it —
	// never gated on ownership — then the new root's own 'router up' adopts
	// a fresh container labelled with its own home.
	if err := first.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Ensure(ctx); err != nil {
		t.Fatalf("expected the second berth root's up to adopt cleanly once the first's is stopped: %v", err)
	}
	adopted := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
	adoptedLabel := dockerOutput(t, "inspect", adopted, "--format", "{{index .Config.Labels \""+router.RouterHomeLabel+"\"}}")
	if adoptedLabel != home2 {
		t.Fatalf("router-home label after takeover = %q, want %q", adoptedLabel, home2)
	}
}

func TestRouterUpRefusesARouterWithoutAnOwnerLabel(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-unowned-" + id
	home := registrytest.TempDir(t)
	state := newState(filepath.Join(home, "router"))
	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
	})

	// A router container this berth did not start: its project and service
	// are the router's, but it carries no owner label at all, as a real
	// daemon reports one — the key absent from .Config.Labels, not empty.
	dir := registrytest.TempDir(t)
	compose := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(compose, []byte("services:\n  router:\n    image: busybox:1.37.0\n    command: [\"sleep\", \"600\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "compose", "-p", project, "-f", compose, "up", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	before := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
	if label := dockerOutput(t, "inspect", before, "--format", "{{index .Config.Labels \""+router.RouterHomeLabel+"\"}}"); label != "" {
		t.Fatalf("fixture router carries an owner label %q; it must carry none", label)
	}

	svc, err := router.New(router.Options{
		State: state, Port: freePortForTest(t), Runner: osrun.New(), Logger: logger.Silent{},
		ProjectName: project, Network: project, Environ: os.Environ(), Home: home, CommandName: "berth",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "The shared router is not owned by this berth root.") {
		t.Fatalf("got %v, want a refusal of a router without an owner label", err)
	}
	if after := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router"); after != before {
		t.Fatalf("expected the refusal to leave the router untouched, got %q then %q", before, after)
	}
}

// TestRouterLockContention is #12: two "control homes" alternate `router
// up`; the second waits on the single machine-wide lock rather than racing
// the first. Uses a fake docker so the contention itself — real goroutines
// against a real flock — is what is under test, not the daemon.
func TestRouterLockContention(t *testing.T) {
	dir := registrytest.TempDir(t)
	state := newState(dir)
	scope := lock.Scope{Target: state.LockPath, Description: "shared router"}

	var mu sync.Mutex
	var order []string
	record := func(event string) {
		mu.Lock()
		order = append(order, event)
		mu.Unlock()
	}

	// acquired is closed the instant a holdRouter call actually holds the
	// lock, so a second caller can be started exactly then — never a guess
	// at how long the first will take to get there.
	holdRouter := func(label string, hold time.Duration, acquired chan<- struct{}) error {
		fake := &sequencedFake{label: label, hold: hold, record: record, state: state, port: 80}
		svc, err := router.New(router.Options{State: state, Port: 80, Runner: fake, Logger: logger.Silent{}})
		if err != nil {
			return err
		}
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Timeout: 10 * time.Second})
		ctx, err := lk.Acquire(context.Background(), "test router up "+label)
		if err != nil {
			return err
		}
		if acquired != nil {
			close(acquired)
		}
		defer lk.Release()
		return svc.Ensure(ctx)
	}

	aAcquired := make(chan struct{})
	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(1)
	go func() {
		defer wg.Done()
		errA = holdRouter("A", 300*time.Millisecond, aAcquired)
	}()
	<-aAcquired // B only starts once A genuinely holds the lock, not after a guessed delay
	wg.Add(1)
	go func() {
		defer wg.Done()
		errB = holdRouter("B", 0, nil)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("A: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B: %v", errB)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	aEnd, bStart := -1, -1
	for i, event := range got {
		switch event {
		case "end:A":
			aEnd = i
		case "start:B":
			if bStart == -1 {
				bStart = i
			}
		}
	}
	if aEnd == -1 || bStart == -1 || bStart < aEnd {
		t.Fatalf("expected B's docker calls to wait for A's to finish under the single lock; order = %v", got)
	}
}

// sequencedFake answers as a healthy, current-protocol router (so Ensure's
// fast path is taken) and records when its `up` call starts and ends, after
// sleeping hold — the seam TestRouterLockContention uses to prove ordering.
type sequencedFake struct {
	label  string
	hold   time.Duration
	record func(string)
	state  layout.RouterLayout
	port   int
}

// Which always answers true.
func (f *sequencedFake) Which(string) bool { return true }

// Run answers as a healthy, current-protocol router, recording when its `up`
// call starts and ends around a sleep of f.hold.
func (f *sequencedFake) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	args := spec.Args
	switch {
	case len(args) > 0 && args[0] == "compose" && containsArg(args, "up"):
		f.record("start:" + f.label)
		time.Sleep(f.hold)
		f.record("end:" + f.label)
		return run.Result{}, nil
	case len(args) > 0 && args[0] == "compose" && containsArg(args, "ps"):
		return run.Result{Stdout: "c-" + args[len(args)-1]}, nil
	case len(args) > 0 && args[0] == "inspect":
		service := strings.TrimPrefix(args[1], "c-")
		var doc router.ContainerInspect
		switch service {
		case router.RouterService:
			doc = routerDoc(f.state.DynamicDir, f.port)
		default:
			doc = indexDoc(f.state.IndexDir)
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

// TestRouterReconnectsLabelledWorktreeNetworks is #55: a worktree's own
// dedicated router network, labelled as this router's own after the router
// already exists (exactly the order `up` uses: runtime.Up creates and
// labels the network before ConnectNetwork ever runs), gets reconnected by
// the router's own next Ensure — not only right after a recreate, since
// Ensure runs the reconnection step unconditionally. ConnectNetwork and
// DisconnectNetwork are exercised directly too, proving both tolerate being
// called against an already-attached or already-detached pair.
func TestRouterReconnectsLabelledWorktreeNetworks(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-reconnect-" + id
	ns := "dev.berth-test-router-reconnect-" + id
	dir := registrytest.TempDir(t)
	port := freePortForTest(t)
	state := newState(dir)
	workNet := "berth-router-test-worktree-" + id

	t.Cleanup(func() {
		// The router container must be gone (or at least disconnected)
		// before workNet can be removed — Docker refuses to remove a
		// network with an active endpoint on it, and a fatal assertion
		// between the Ensure below that attaches the router to workNet and
		// this test's own explicit DisconnectNetwork call would otherwise
		// leave it attached when cleanup runs.
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
		exec.Command("docker", "network", "rm", workNet).Run()
		exec.Command("docker", "network", "rm", project).Run()
	})

	svc, err := router.New(router.Options{
		State: state, Port: port, Runner: osrun.New(), Logger: logger.Silent{},
		ProjectName: project, Network: project, LabelNamespace: ns, Environ: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate what internal/backend's overrideDocument declares for a
	// routed worktree: an owned network labelled with RoutedByLabel naming
	// this router's own shared network.
	dockerOutput(t, "network", "create", "--label", router.RoutedByLabel+"="+project, workNet)

	routerID := dockerOutput(t, "compose", "-p", project, "ps", "-q", "router")
	before := dockerOutput(t, "inspect", routerID, "--format", "{{json .NetworkSettings.Networks}}")
	if strings.Contains(before, workNet) {
		t.Fatalf("expected the router not to be attached to %s yet, got %s", workNet, before)
	}

	// Ensure's own reconnection step — the same one `router up` runs, and
	// the same one an `up` racing a concurrent recreate relies on to
	// self-heal (see ConnectNetwork's own comment) — must pick this up on
	// its own, with nothing else calling ConnectNetwork directly.
	if err := svc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	after := dockerOutput(t, "inspect", routerID, "--format", "{{json .NetworkSettings.Networks}}")
	if !strings.Contains(after, workNet) {
		t.Fatalf("expected the router to be reconnected to %s, got %s", workNet, after)
	}

	// ConnectNetwork itself tolerates an already-attached pair.
	if err := svc.ConnectNetwork(ctx, workNet); err != nil {
		t.Fatalf("ConnectNetwork on an already-attached network: %v", err)
	}

	// DisconnectNetwork detaches it, and tolerates being called again once
	// it already has been — the "nothing to disconnect" case `down` and
	// `worktree remove` rely on being harmless.
	if err := svc.DisconnectNetwork(ctx, workNet); err != nil {
		t.Fatal(err)
	}
	if err := svc.DisconnectNetwork(ctx, workNet); err != nil {
		t.Fatalf("DisconnectNetwork on an already-detached network: %v", err)
	}
	detached := dockerOutput(t, "inspect", routerID, "--format", "{{json .NetworkSettings.Networks}}")
	if strings.Contains(detached, workNet) {
		t.Fatalf("expected the router to be disconnected from %s, got %s", workNet, detached)
	}

	// Verify itself does not report the disconnected-but-still-labelled
	// network as a problem (found in review, round 2 of #55, superseding
	// this test's own earlier assertion that it did): an unattached
	// labelled network is not a stack problem. It is exactly the state a
	// concurrent `down` or `worktree remove` can leave this router in for a
	// moment — the window this test's own DisconnectNetwork call above
	// simulates — or a worktree between its own `up` creating the network
	// and this router's next Ensure reconnecting it; treating either as
	// drift would make status and doctor fail, and could make Ensure
	// force-recreate a perfectly healthy router, over a race it cannot win
	// by recreating. Ensure's own reconnectWorktreeNetworks, exercised
	// again below, is the actual repair.
	problems, err := svc.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		if strings.Contains(problem, "attached to networks") {
			t.Fatalf("expected Verify not to flag an unattached labelled network, got %v", problems)
		}
	}

	// A repeated Ensure reconnects it again regardless, proving the repair
	// path still works even though Verify itself no longer treats the gap
	// as drift worth a warning or a force-recreate.
	if err := svc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	reattached := dockerOutput(t, "inspect", routerID, "--format", "{{json .NetworkSettings.Networks}}")
	if !strings.Contains(reattached, workNet) {
		t.Fatalf("expected a repeated Ensure to reconnect %s again, got %s", workNet, reattached)
	}
}

// TestRouterUpTimesOutOnAHungDocker is #14: a `docker` that never answers
// must not hang `router up` forever. The fake docker is a real, hanging
// subprocess — found only through this test's own PATH override — so the
// real timeout-and-kill machinery in internal/run/osrun is what is exercised.
func TestRouterUpTimesOutOnAHungDocker(t *testing.T) {
	id := suffix(t)
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nsleep 999999\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	state := newState(registrytest.TempDir(t))
	svc, err := router.New(router.Options{
		State: state, Port: freePortForTest(t), Runner: osrun.New(), Logger: logger.Silent{},
		ProjectName: "berth-router-test-" + id, Network: "berth-router-test-" + id,
		Environ:  []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")},
		Timeouts: router.Timeouts{Probe: 300 * time.Millisecond, Start: 300 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err = svc.Ensure(context.Background())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected the hung `docker` to be caught as a failure rather than hanging Ensure forever")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("expected the timeout to end this well under 10s, took %s (err: %v)", elapsed, err)
	}
	t.Logf("Ensure failed after %s, as expected: %v", elapsed, err)
}
