//go:build integration

package router_test

// Ported from tests/integration/router-docker.integration.test.ts, plus new
// tests for the owner decisions this work package covers (#12, #13, #14):
// every fixture and expectation uses a unique project, network, port and
// state directory, and cleans up after itself, so it can run beside another
// developer's real router and beside another track's Docker suite on the
// same daemon.
//
// Not ported: "routes a worktree by hostname..." and "lets doctor read the
// running router...". Both need machinery two other, parallel work packages
// own and had not landed here yet: WP2's Compose runtime `up`/override
// generation (a worktree's own routing) and the future `doctor` port. "keeps
// the shared network on down while a worktree is attached" is adapted below
// to attach a plain container to the shared network directly instead of
// through a full worktree — the same router behaviour, reached without that
// dependency (see its own comment).

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

// freePortForTest asks the kernel for a free loopback port, the way the
// TypeScript suite's freePort() helper does.
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

	t.Run("exposes only the read-only Docker API through the proxy", func(t *testing.T) {
		network := project + "_docker-api"
		allowed := dockerOutput(t, "run", "--rm", "--network", network, "busybox:1.37.0", "wget", "-qO-", "http://docker-api:2375/version")
		if !strings.Contains(allowed, "ApiVersion") {
			t.Fatalf("version response = %q", allowed)
		}

		deniedOut, err := exec.Command("docker", "run", "--rm", "--network", network, "busybox:1.37.0",
			"wget", "-qO-", "--post-data=", "http://docker-api:2375/containers/create").CombinedOutput()
		if err == nil {
			t.Fatal("expected POST to be denied")
		}
		if !strings.Contains(string(deniedOut), "403") {
			t.Errorf("denied output = %s", deniedOut)
		}

		if err := exec.Command("docker", "run", "--rm", "--network", network, "busybox:1.37.0",
			"wget", "-qO-", "http://docker-api:2375/info").Run(); err == nil {
			t.Error("expected /info to be denied")
		}
	})

	// Adapted from the TypeScript suite's worktree-routing scenario, which
	// attached a worktree's own containers to prove the shared network stays
	// while something is attached and goes once nothing is: WP2's Compose
	// runtime (`up`, override generation) is a parallel work package not yet
	// available here. A plain container joins the network directly instead,
	// exercising the same router behaviour without that dependency. Runs
	// last: it stops the router itself.
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

// TestTwoControlHomesShareOneRouter is #12/#13: the router's state lives
// outside every control home, one per user account, so two control homes
// resolving to the same state directory share one router — the second
// `ensure` from what is, in production, a different control home's process
// must not recreate anything.
func TestTwoControlHomesShareOneRouter(t *testing.T) {
	requireDocker(t)
	id := suffix(t)
	project := "berth-router-test-two-" + id
	ns := "dev.berth-test-router-two-" + id
	// One state directory — what layout.RouterState resolves to for one
	// user account regardless of which control home (BERTH_HOME) is asking.
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
		case router.DockerAPIService:
			doc = apiDoc()
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
