//go:build integration

package router_test

// Real-daemon tests of the shared router: the proxy in host and bridge mode,
// the ports it needs checked before it starts, sharing across control homes
// (#12, #13), ownership (#82), lock contention and timeouts (#14). Every
// fixture uses a unique Compose project, label namespace, router port and
// scratch treerig root, and cleans up after itself, so it can run beside a
// developer's real router and beside another Docker suite on the same
// daemon. Ports for the checkout index and the host-mode ping come from a
// range of their own inside the test range, never the default one. Tests
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/osrun"
)

// routerTestRange is where these tests' router allocates the index and ping
// ports from: inside the test range, apart from the sub-range the `up` suites
// use by default.
var routerTestRange = layout.PortRange{Low: 30900, High: 30999}

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

// requireDocker skips t (or fails it, under TREERIG_TEST_REQUIRE_DOCKER=1)
// when no Docker daemon is reachable. TREERIG_TEST_SKIP_DOCKER=1 skips it even
// when a daemon is reachable, so CI can leave every Docker test to its own
// job.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("TREERIG_TEST_SKIP_DOCKER") == "1" {
		t.Skip("Docker tests are skipped (TREERIG_TEST_SKIP_DOCKER=1)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("TREERIG_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (TREERIG_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

// requireNativeLinuxEngine skips t unless the client is Linux and the daemon
// reports a rootful Linux engine that is not Docker Desktop or OrbStack: the
// engine on which mode detection picks host, and host networking is the
// machine's own.
func requireNativeLinuxEngine(t *testing.T) {
	t.Helper()
	requireDocker(t)
	if runtime.GOOS != "linux" {
		t.Skip("host mode needs a Linux client")
	}
	out, err := exec.Command("docker", "info", "--format", "{{.OperatingSystem}}|{{.Name}}|{{.SecurityOptions}}").Output()
	if err != nil {
		t.Skipf("docker info: %v", err)
	}
	info := strings.ToLower(string(out))
	if strings.Contains(info, "docker desktop") || strings.Contains(info, "orbstack") || strings.Contains(info, "rootless") {
		t.Skipf("not a native rootful Linux engine: %s", strings.TrimSpace(info))
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

// routerClient gives up on one request quickly: a route the proxy has not
// loaded yet answers at once, and one that never will should be asked again,
// not waited on.
var routerClient = &http.Client{Timeout: 5 * time.Second}

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
	resp, err := routerClient.Do(req)
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

// liveAnswer is one HTTP response, reduced to what the assertions compare.
type liveAnswer struct {
	Status int
	Body   string
}

// eventually retries probe (Traefik's file provider picks a change up after
// a throttle of about 2 s) until accept is satisfied or timeout elapses.
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

// routeAnswers waits for the proxy on port to answer host with want.
func routeAnswers(t *testing.T, port int, host string, want liveAnswer) {
	t.Helper()
	eventually(t, 30*time.Second,
		func() liveAnswer {
			status, body := fetchThroughRouter(t, port, host, "/")
			return liveAnswer{status, body}
		},
		func(a liveAnswer) bool { return a.Status == want.Status && strings.Contains(a.Body, want.Body) })
}

// liveRouter is one real router: its own project, label namespace, scratch treerig
// root and Docker cleanup.
type liveRouter struct {
	t           *testing.T
	project, ns string
	home        string
	controlHome string
	state       layout.RouterLayout
}

// newLiveRouter is a scratch treerig root and a project of its own, with a cleanup
// that removes the project's containers and default network.
func newLiveRouter(t *testing.T) *liveRouter {
	t.Helper()
	requireDocker(t)
	id := suffix(t)
	home := registrytest.TempDir(t)
	w := &liveRouter{
		t: t, project: testProjectPrefix + id, ns: "dev.treerig-test-router-" + id,
		home: home, controlHome: filepath.Join(home, "control-home"), state: layout.RouterStateFrom(home),
	}
	sweepOnce.Do(sweepAbandonedRouters)
	t.Cleanup(func() {
		// `down` first, so Compose stops things in order; whether or not it
		// worked, the project is then removed by its compose label, which is how
		// Compose itself finds its containers and networks.
		_ = exec.Command("docker", "compose", "-p", w.project, "down", "--remove-orphans").Run()
		removeProjectByLabel(w.project)
	})
	return w
}

// testProjectPrefix is the Compose project name prefix every router test uses.
const testProjectPrefix = "treerig-router-test-"

// composeProjectLabel is the label Compose stamps on everything of a project.
const composeProjectLabel = "com.docker.compose.project"

// sweepOnce runs the start-of-run sweep of abandoned routers once per process.
var sweepOnce sync.Once

// removeProjectByLabel force-removes the containers and networks that carry
// project's Compose label. It removes by label only, never by name.
func removeProjectByLabel(project string) {
	filter := "label=" + composeProjectLabel + "=" + project
	if out, err := exec.Command("docker", "ps", "-aq", "--filter", filter).Output(); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.Command("docker", append([]string{"rm", "--force"}, ids...)...).Run()
		}
	}
	if out, err := exec.Command("docker", "network", "ls", "-q", "--filter", filter).Output(); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.Command("docker", append([]string{"network", "rm"}, ids...)...).Run()
		}
	}
}

// sweepAbandonedRouters removes what a killed earlier run left behind:
// containers whose Compose project starts with testProjectPrefix and whose
// router-home label names a path that no longer exists. A running test's
// router-home is its live scratch root, so it is never touched. Removal is by
// labels only.
func sweepAbandonedRouters() {
	format := `{{.ID}}|{{.Label "` + composeProjectLabel + `"}}|{{.Label "` + router.RouterHomeLabel + `"}}`
	out, err := exec.Command("docker", "ps", "-a", "--format", format).Output()
	if err != nil {
		return
	}
	projects := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) != 3 || !strings.HasPrefix(fields[1], testProjectPrefix) || fields[2] == "" {
			continue
		}
		if _, err := os.Stat(fields[2]); os.IsNotExist(err) {
			projects[fields[1]] = true
		}
	}
	for project := range projects {
		removeProjectByLabel(project)
	}
}

// router builds a Router over this world; configure may change its options
// before it is built.
func (w *liveRouter) router(port int, mode string, configure func(*router.Options)) (*router.Router, *logger.Recording) {
	w.t.Helper()
	rec := &logger.Recording{}
	options := router.Options{
		State: w.state, Port: port, Runner: osrun.New(), Logger: rec,
		ProjectName: w.project, LabelNamespace: w.ns, Environ: os.Environ(),
		CommandName: "treerig", Home: w.home, ControlHome: w.controlHome,
		PortRange: routerTestRange, ModeOverride: mode, ClientOS: runtime.GOOS,
	}
	if configure != nil {
		configure(&options)
	}
	svc, err := router.New(options)
	if err != nil {
		w.t.Fatal(err)
	}
	return svc, rec
}

// containerID is the router stack's container of service, "" when none.
func (w *liveRouter) containerID(service string) string {
	w.t.Helper()
	return dockerOutput(w.t, "compose", "-p", w.project, "ps", "-q", service)
}

// liveInspected is the part of `docker inspect` these tests read.
type liveInspected struct {
	Config struct {
		User   string
		Labels map[string]string
		Cmd    []string
	}
	HostConfig struct {
		NetworkMode    string
		ReadonlyRootfs bool
		CapDrop        []string
		CapAdd         []string
		SecurityOpt    []string
		Privileged     bool
	}
	Mounts          []struct{ Source, Destination string }
	NetworkSettings struct {
		Ports map[string][]struct{ HostIp, HostPort string }
	}
}

// inspect reads one container.
func (w *liveRouter) inspect(id string) liveInspected {
	w.t.Helper()
	var docs []liveInspected
	if err := json.Unmarshal([]byte(dockerOutput(w.t, "inspect", id)), &docs); err != nil || len(docs) != 1 {
		w.t.Fatalf("inspect %s: %v", id, err)
	}
	return docs[0]
}

// liveListeners are the Local Address:Port columns of `ss -ltn`.
func liveListeners(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss is not installed")
	}
	out, err := exec.Command("ss", "-ltn").Output()
	if err != nil {
		t.Fatalf("ss -ltn: %v", err)
	}
	var local []string
	for i, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) < 4 {
			continue
		}
		local = append(local, fields[3])
	}
	return local
}

// requireLoopbackOnly fails t unless port has a listener on 127.0.0.1 and
// none on any wildcard or other address.
func requireLoopbackOnly(t *testing.T, port int) {
	t.Helper()
	suffix := ":" + strconv.Itoa(port)
	var found []string
	for _, local := range liveListeners(t) {
		if strings.HasSuffix(local, suffix) {
			found = append(found, local)
		}
	}
	if len(found) == 0 {
		t.Fatalf("nothing listens on port %d", port)
	}
	for _, local := range found {
		if local != "127.0.0.1"+suffix {
			t.Errorf("port %d listens on %s, not on 127.0.0.1 alone (all: %v)", port, local, found)
		}
	}
}

// recordPrivilegedListeners logs `sudo -n ss -ltnp` for the given ports, the
// process names an unprivileged ss cannot show; skipped quietly without sudo.
func recordPrivilegedListeners(t *testing.T, ports ...int) {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "ss", "-ltnp").Output()
	if err != nil {
		t.Logf("sudo ss -ltnp unavailable: %v", err)
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, port := range ports {
			if strings.Contains(line, ":"+strconv.Itoa(port)+" ") {
				t.Logf("sudo ss -ltnp: %s", strings.Join(strings.Fields(line), " "))
			}
		}
	}
}

// hostServer serves body on a listener of network at address, for a route to
// target as a process on the host would, and returns its port. Skips t when
// the address cannot be bound (no IPv6, say).
func hostServer(t *testing.T, network, address, body string) int {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("cannot listen on %s %s: %v", network, address, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

// writeRoute writes a route file for host to the loopback port target.
func (w *liveRouter) writeRoute(name, host string, target int) {
	w.t.Helper()
	repository, worktree := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	doc := router.NewFragmentDocument(name, fmt.Sprintf("Host(`%s`)", host), target)
	if err := router.WriteFragment(w.state, repository, worktree, name, doc); err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = router.RemoveWorktreeFragments(w.state, repository, worktree) })
}

// TestSharedRouterInHostModeOnARealDaemon is the proxy on a native Linux
// engine: it starts hardened in host network mode, listens on loopback alone,
// and reaches targets bound to either loopback family.
func TestSharedRouterInHostModeOnARealDaemon(t *testing.T) {
	requireNativeLinuxEngine(t)
	w := newLiveRouter(t)
	port := freePortForTest(t)
	svc, _ := w.router(port, "", nil)
	ctx := context.Background()

	if err := svc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	state, found, err := router.ReadState(w.state)
	if err != nil || !found || state.IndexPort == 0 || state.PingPort == 0 {
		t.Fatalf("router state = %+v, %v, %v", state, found, err)
	}
	if !routerTestRange.Contains(state.IndexPort) || !routerTestRange.Contains(state.PingPort) {
		t.Fatalf("state ports %+v are outside %v", state, routerTestRange)
	}

	t.Run("starts healthy and is left alone by a second ensure", func(t *testing.T) {
		before := w.containerID("router")
		beforeIndex := w.containerID("index")
		if err := svc.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
		if after := w.containerID("router"); after != before {
			t.Fatalf("router container changed: %q then %q", before, after)
		}
		if after := w.containerID("index"); after != beforeIndex {
			t.Fatalf("index container changed: %q then %q", beforeIndex, after)
		}
		status, err := svc.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running || len(status.Problems) != 0 || status.Port != port || status.RunningPort != port || status.Mode != router.ModeHost {
			t.Fatalf("status = %+v", status)
		}
		if report := router.StatusReport(status); len(report.Output) != 1 || !strings.Contains(report.Output[0], fmt.Sprintf("running on 127.0.0.1:%d, host mode", port)) {
			t.Fatalf("report = %+v", report)
		}
		if logs := dockerOutput(t, "logs", before); strings.Contains(logs, "level=error") {
			t.Errorf("expected no errors in the router's logs, got:\n%s", logs)
		}
	})

	t.Run("runs in the host network namespace, hardened, with no Docker socket", func(t *testing.T) {
		doc := w.inspect(w.containerID("router"))
		if doc.HostConfig.NetworkMode != "host" {
			t.Errorf("NetworkMode = %q", doc.HostConfig.NetworkMode)
		}
		if doc.Config.Labels[router.RouterModeLabel] != "host" || doc.Config.Labels[router.RouterPortLabel] != strconv.Itoa(port) {
			t.Errorf("labels = %v", doc.Config.Labels)
		}
		if !doc.HostConfig.ReadonlyRootfs || !slices.Contains(doc.HostConfig.CapDrop, "ALL") || !slices.Contains(doc.HostConfig.SecurityOpt, "no-new-privileges:true") || doc.HostConfig.Privileged {
			t.Errorf("not hardened: %+v", doc.HostConfig)
		}
		// A high port needs no privilege: nobody, and no capability back.
		if doc.Config.User != "65534:65534" || len(doc.HostConfig.CapAdd) != 0 {
			t.Errorf("user %q, cap_add %v; want nobody and none", doc.Config.User, doc.HostConfig.CapAdd)
		}
		for _, mount := range doc.Mounts {
			if strings.Contains(mount.Source, "docker.sock") {
				t.Errorf("the router mounts %s", mount.Source)
			}
		}
		if len(doc.Mounts) != 1 || doc.Mounts[0].Destination != router.DynamicConfigMount {
			t.Errorf("mounts = %+v, want only the dynamic-configuration directory", doc.Mounts)
		}
		for _, argument := range doc.Config.Cmd {
			if strings.Contains(argument, "providers.docker") {
				t.Errorf("Docker provider flag %q", argument)
			}
		}
	})

	t.Run("listens on 127.0.0.1 alone: no wildcard listener on the router or ping port", func(t *testing.T) {
		requireLoopbackOnly(t, port)
		requireLoopbackOnly(t, state.PingPort)
		requireLoopbackOnly(t, state.IndexPort)
		recordPrivilegedListeners(t, port, state.PingPort, state.IndexPort)
	})

	t.Run("reaches a 127.0.0.1-only host process, a ::1-only one, and answers 502 for a dead target", func(t *testing.T) {
		v4 := hostServer(t, "tcp4", "127.0.0.1:0", "from v4\n")
		w.writeRoute("v4", "v4.invalid.localhost", v4)
		routeAnswers(t, port, "v4.invalid.localhost", liveAnswer{http.StatusOK, "from v4"})

		// treerig-loopback maps to ::1 as well, so a listener bound only there is reached.
		v6 := hostServer(t, "tcp6", "[::1]:0", "from v6\n")
		w.writeRoute("v6", "v6.invalid.localhost", v6)
		routeAnswers(t, port, "v6.invalid.localhost", liveAnswer{http.StatusOK, "from v6"})

		w.writeRoute("dead", "dead.invalid.localhost", freePortForTest(t))
		routeAnswers(t, port, "dead.invalid.localhost", liveAnswer{http.StatusBadGateway, ""})
	})

	t.Run("answers an unmatched host with its own 404, and serves ping on its own entrypoint, never on web", func(t *testing.T) {
		status, body := fetchThroughRouter(t, port, "nobody.invalid.localhost", "/")
		if status != http.StatusNotFound || !strings.HasPrefix(body, "404 page not found") {
			t.Fatalf("unmatched host answered %d %q", status, body)
		}
		if status, body := fetchThroughRouter(t, port, "nobody.invalid.localhost", "/ping"); status != http.StatusNotFound {
			t.Fatalf("/ping on the web entrypoint answered %d %q", status, body)
		}
		resp, err := routerClient.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", state.PingPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if text, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || strings.TrimSpace(string(text)) != "OK" {
			t.Fatalf("ping entrypoint answered %d %q", resp.StatusCode, text)
		}
	})

	t.Run("serves the checkout index through the proxy, and publishes it on its own loopback port", func(t *testing.T) {
		routeAnswers(t, port, router.IndexHost, liveAnswer{http.StatusOK, "No checkouts have been indexed yet"})
		doc := w.inspect(w.containerID("index"))
		bindings := doc.NetworkSettings.Ports["8080/tcp"]
		if len(bindings) != 1 || bindings[0].HostIp != "127.0.0.1" || bindings[0].HostPort != strconv.Itoa(state.IndexPort) {
			t.Fatalf("index bindings = %+v, want 127.0.0.1:%d", bindings, state.IndexPort)
		}
		resp, err := routerClient.Get(fmt.Sprintf("http://127.0.0.1:%d/index.html", state.IndexPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the index's own port answered %d", resp.StatusCode)
		}
	})

	t.Run("goes away on down", func(t *testing.T) {
		if err := svc.Down(ctx); err != nil {
			t.Fatal(err)
		}
		if id := w.containerID("router"); id != "" {
			t.Fatalf("router container %s survived down", id)
		}
		status, err := svc.Status(ctx)
		if err != nil || status.Running {
			t.Fatalf("status = %+v, %v", status, err)
		}
	})
}

// TestSharedRouterInBridgeModeOnARealDaemon forces bridge mode, as Docker
// Desktop and OrbStack would be detected: the wiring is proven here, the
// reach to a loopback target through host-gateway is not (it does not work
// on a plain Linux engine; docs/design/routing.md says so).
func TestSharedRouterInBridgeModeOnARealDaemon(t *testing.T) {
	w := newLiveRouter(t)
	port := freePortForTest(t)
	svc, _ := w.router(port, "bridge", nil)
	ctx := context.Background()
	if err := svc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}

	doc := w.inspect(w.containerID("router"))
	if doc.HostConfig.NetworkMode == "host" {
		t.Errorf("NetworkMode = %q, want a bridge network", doc.HostConfig.NetworkMode)
	}
	if doc.Config.Labels[router.RouterModeLabel] != "bridge" || doc.Config.Labels[router.RouterPortLabel] != strconv.Itoa(port) {
		t.Errorf("labels = %v", doc.Config.Labels)
	}
	// Published on 127.0.0.1 alone, and nothing else: the ping entrypoint is inside.
	published := 0
	for container, bindings := range doc.NetworkSettings.Ports {
		for _, binding := range bindings {
			published++
			if container != "80/tcp" || binding.HostIp != "127.0.0.1" || binding.HostPort != strconv.Itoa(port) {
				t.Errorf("published %s -> %s:%s", container, binding.HostIp, binding.HostPort)
			}
		}
	}
	if published != 1 {
		t.Errorf("published %d ports, want exactly 127.0.0.1:%d", published, port)
	}
	if !slices.Contains(doc.HostConfig.CapDrop, "ALL") || !doc.HostConfig.ReadonlyRootfs {
		t.Errorf("not hardened: %+v", doc.HostConfig)
	}
	status, err := svc.Status(ctx)
	if err != nil || !status.Running || len(status.Problems) != 0 || status.Mode != router.ModeBridge {
		t.Fatalf("status = %+v, %v", status, err)
	}
	// The unrouted host gets the proxy's own 404, and the index is routed.
	if status, body := fetchThroughRouter(t, port, "nobody.invalid.localhost", "/"); status != http.StatusNotFound || !strings.HasPrefix(body, "404 page not found") {
		t.Fatalf("unmatched host answered %d %q", status, body)
	}
	if state, _, err := router.ReadState(w.state); err != nil || state.IndexPort == 0 {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

// TestRouterModeAndPortChangesRecreateTheProxy is decision 1: a different
// mode or router port changes the proxy's configuration, so Compose
// recreates it, and a moved port is said once.
func TestRouterModeAndPortChangesRecreateTheProxy(t *testing.T) {
	requireNativeLinuxEngine(t)
	w := newLiveRouter(t)
	port := freePortForTest(t)
	ctx := context.Background()

	host, _ := w.router(port, "", nil)
	if err := host.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	hostID := w.containerID("router")

	bridge, _ := w.router(port, "bridge", nil)
	if err := bridge.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	bridgeID := w.containerID("router")
	if bridgeID == hostID || w.inspect(bridgeID).Config.Labels[router.RouterModeLabel] != "bridge" {
		t.Fatalf("host -> bridge kept container %s (%v)", bridgeID, w.inspect(bridgeID).Config.Labels)
	}

	again, _ := w.router(port, "", nil)
	if err := again.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	againID := w.containerID("router")
	if againID == bridgeID || w.inspect(againID).Config.Labels[router.RouterModeLabel] != "host" {
		t.Fatalf("bridge -> host kept container %s", againID)
	}

	moved := freePortForTest(t)
	other, rec := w.router(moved, "", nil)
	if err := other.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	movedID := w.containerID("router")
	if movedID == againID || w.inspect(movedID).Config.Labels[router.RouterPortLabel] != strconv.Itoa(moved) {
		t.Fatalf("changing the port kept container %s", movedID)
	}
	lines, _ := rec.Snapshot()
	if !slices.ContainsFunc(lines, func(line string) bool {
		return strings.Contains(line, fmt.Sprintf("moved from port %d to %d", port, moved))
	}) {
		t.Errorf("no port-change note in %v", lines)
	}
	requireLoopbackOnly(t, moved)
}

// TestEnsureChecksPortsBeforeStarting is decision 10's preflight: a port the
// router needs that something else holds is refused before anything starts,
// naming the holder.
func TestEnsureChecksPortsBeforeStarting(t *testing.T) {
	w := newLiveRouter(t)
	ctx := context.Background()
	noContainers := func(t *testing.T) {
		t.Helper()
		if ids := dockerOutput(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+w.project); ids != "" {
			t.Fatalf("a refused start left containers behind: %s", ids)
		}
	}

	t.Run("names another process holding the router port", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		held := listener.Addr().(*net.TCPAddr).Port
		svc, _ := w.router(held, "", nil)
		err = svc.Ensure(ctx)
		want := fmt.Sprintf("127.0.0.1:%d (the router) is held by another process", held)
		if err == nil || !strings.Contains(err.Error(), "Cannot start the shared router") || !strings.Contains(err.Error(), want) {
			t.Fatalf("Ensure = %v, want a refusal containing %q", err, want)
		}
		noContainers(t)
	})

	t.Run("names the container publishing the router port", func(t *testing.T) {
		held := freePortForTest(t)
		name := "treerig-router-test-holder-" + suffix(t)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", name).Run() })
		dockerOutput(t, "run", "--detach", "--name", name, "--publish", fmt.Sprintf("127.0.0.1:%d:80", held),
			"busybox:1.37.0", "sh", "-c", "exec httpd -f -p 80")
		svc, _ := w.router(held, "", nil)
		err := svc.Ensure(ctx)
		if err == nil || !strings.Contains(err.Error(), "is held by container "+name) {
			t.Fatalf("Ensure = %v, want the container %s named", err, name)
		}
		noContainers(t)
	})
}

// TestFailedStartShowsTheContainersLastLogLines is decision 10's other half:
// when a start comes up unhealthy the failure carries the containers' own
// log lines. The preflight is blinded with a probe that calls every port
// free, so a port that is really taken gets as far as the proxy's own bind.
func TestFailedStartShowsTheContainersLastLogLines(t *testing.T) {
	requireNativeLinuxEngine(t)
	w := newLiveRouter(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	held := listener.Addr().(*net.TCPAddr).Port
	svc, _ := w.router(held, "host", func(o *router.Options) {
		o.Probe = registrytest.FreeProbe{}
		o.Timeouts = router.Timeouts{Start: 90 * time.Second}
	})
	err = svc.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected the start to fail with its port taken")
	}
	if !strings.Contains(err.Error(), "The shared router failed to start.") || !strings.Contains(err.Error(), "Last log lines:") {
		t.Fatalf("Ensure = %v, want the failure and the last log lines", err)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("the log lines do not show why: %v", err)
	}
}

// TestRouterRefusesARunningRouterOfAnotherLayout stages a running router
// container that speaks a lower protocol, as one routing through per-worktree
// networks does: Ensure refuses it with "router down" advice and touches
// nothing.
func TestRouterRefusesARunningRouterOfAnotherLayout(t *testing.T) {
	w := newLiveRouter(t)
	compose := filepath.Join(registrytest.TempDir(t), "compose.yaml")
	contents := fmt.Sprintf(`services:
  router:
    image: busybox:1.37.0
    init: true
    command: ["sleep", "600"]
    labels:
      %q: %q
      %q: %q
`, w.ns+".router-protocol", strconv.Itoa(router.Protocol-1), router.RouterHomeLabel, w.home)
	if err := os.WriteFile(compose, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "compose", "-p", w.project, "-f", compose, "up", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	before := w.containerID("router")

	svc, _ := w.router(freePortForTest(t), "", nil)
	err := svc.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "router down") {
		t.Fatalf("Ensure = %v, want a refusal telling the user to run 'treerig router down' first", err)
	}
	if after := w.containerID("router"); after != before {
		t.Fatalf("the refusal touched the router: %q then %q", before, after)
	}
	if index := w.containerID("index"); index != "" {
		t.Fatalf("the refusal started an index container %s", index)
	}
}

// TestTwoControlHomesShareOneRouter is #12/#13/#80: the router's state lives
// under the treerig root (TREERIG_HOME), outside every control home, so two
// control homes under the same treerig root resolve to the same state
// directory and share one router; the second `ensure` from what is, in
// production, a different control home's process must not recreate anything.
func TestTwoControlHomesShareOneRouter(t *testing.T) {
	requireNativeLinuxEngine(t)
	w := newLiveRouter(t)
	port := freePortForTest(t)
	ctx := context.Background()

	first, _ := w.router(port, "", nil)
	if err := first.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	before := w.containerID("router")

	// A second control home's own process, its own *router.Router, same state.
	second, _ := w.router(port, "", func(o *router.Options) { o.ControlHome = filepath.Join(w.home, "other-control-home") })
	if err := second.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if after := w.containerID("router"); before != after {
		t.Fatalf("expected the second control home's ensure to find the same router, got %q then %q", before, after)
	}
}

// TestTwoTreerigRootsRouterUpRefuses is #82: two different treerig roots each
// resolve their own, separate router state, but the router container itself
// is still one per Docker daemon (its Compose project name is fixed
// regardless of treerig root). The second treerig root's Ensure refuses, naming
// the first, and leaves the first's own running container exactly as it was
// — then 'router down' followed by the new root's own 'router up' hands it
// over cleanly.
func TestTwoTreerigRootsRouterUpRefuses(t *testing.T) {
	requireNativeLinuxEngine(t)
	first := newLiveRouter(t)
	port := freePortForTest(t)
	// A second treerig root: its own state and home label, the same project.
	home2 := registrytest.TempDir(t)
	second := &liveRouter{t: t, project: first.project, ns: first.ns, home: home2, controlHome: filepath.Join(home2, "control-home"), state: layout.RouterStateFrom(home2)}

	ctx := context.Background()
	one, _ := first.router(port, "", nil)
	if err := one.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	before := first.containerID("router")

	two, _ := second.router(port, "", nil)
	err := two.Ensure(ctx)
	want := fmt.Sprintf("The shared router belongs to a different treerig root (%s).", first.home)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want a refusal containing %q", err, want)
	}
	after := first.containerID("router")
	if before != after {
		t.Fatalf("expected the second treerig root's refusal to leave the first's router untouched, got %q then %q", before, after)
	}
	if label := first.inspect(after).Config.Labels[router.RouterHomeLabel]; label != first.home {
		t.Fatalf("router-home label = %q, want %q", label, first.home)
	}

	// The explicit way out (#82): 'router down' stops it whoever owns it,
	// then the new root's own 'router up' adopts a fresh container labelled
	// with its own home.
	if err := one.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if err := two.Ensure(ctx); err != nil {
		t.Fatalf("expected the second treerig root's up to adopt cleanly once the first's is stopped: %v", err)
	}
	if label := first.inspect(first.containerID("router")).Config.Labels[router.RouterHomeLabel]; label != home2 {
		t.Fatalf("router-home label after takeover = %q, want %q", label, home2)
	}
}

// TestRouterUpRefusesARouterWithoutAnOwnerLabel stages a router container
// this treerig did not start: its project and service are the router's, but it
// carries no owner label at all.
func TestRouterUpRefusesARouterWithoutAnOwnerLabel(t *testing.T) {
	w := newLiveRouter(t)
	compose := filepath.Join(registrytest.TempDir(t), "compose.yaml")
	if err := os.WriteFile(compose, []byte("services:\n  router:\n    image: busybox:1.37.0\n    init: true\n    command: [\"sleep\", \"600\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "compose", "-p", w.project, "-f", compose, "up", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	before := w.containerID("router")
	if _, present := w.inspect(before).Config.Labels[router.RouterHomeLabel]; present {
		t.Fatal("fixture router carries an owner label; it must carry none")
	}

	svc, _ := w.router(freePortForTest(t), "", nil)
	err := svc.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "The shared router is not owned by this treerig root.") {
		t.Fatalf("got %v, want a refusal of a router without an owner label", err)
	}
	if after := w.containerID("router"); after != before {
		t.Fatalf("expected the refusal to leave the router untouched, got %q then %q", before, after)
	}
}

// TestRouterLockContention is #12: two "control homes" alternate `router
// up`; the second waits on the single machine-wide lock rather than racing
// the first. Uses a fake docker so the contention itself — real goroutines
// against a real flock — is what is under test, not the daemon.
func TestRouterLockContention(t *testing.T) {
	home := registrytest.TempDir(t)
	state := layout.RouterStateFrom(home)
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
		fake := &sequencedFake{label: label, hold: hold, record: record}
		svc, err := router.New(router.Options{
			State: state, Port: 8081, Runner: fake, Logger: logger.Silent{}, Home: home,
			ControlHome: filepath.Join(home, "control-home"), PortRange: routerTestRange,
			Probe: registrytest.FreeProbe{}, ModeOverride: "host", DockerHost: "unix:///var/run/docker.sock", ClientOS: "linux",
		})
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

// sequencedFake answers as a Linux engine with no router yet, and records
// when its `compose up` call starts and ends, after sleeping hold — the seam
// TestRouterLockContention uses to prove ordering.
type sequencedFake struct {
	label  string
	hold   time.Duration
	record func(string)
}

// Which always answers true.
func (f *sequencedFake) Which(string) bool { return true }

// Run answers `docker info` as a native Linux engine, every `compose ps` with
// no container, and times `compose up` around a sleep of f.hold.
func (f *sequencedFake) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	args := spec.Args
	switch {
	case len(args) > 0 && args[0] == "info":
		return run.Result{Stdout: `{"OSType":"linux","OperatingSystem":"Ubuntu 24.04 LTS","Name":"host","SecurityOptions":["name=seccomp"]}`}, nil
	case len(args) > 0 && args[0] == "compose" && slices.Contains(args, "up"):
		f.record("start:" + f.label)
		time.Sleep(f.hold)
		f.record("end:" + f.label)
		return run.Result{}, nil
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

	home := registrytest.TempDir(t)
	svc, err := router.New(router.Options{
		State: layout.RouterStateFrom(home), Port: freePortForTest(t), Runner: osrun.New(), Logger: logger.Silent{},
		ProjectName: "treerig-router-test-" + id, ControlHome: filepath.Join(home, "control-home"),
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
