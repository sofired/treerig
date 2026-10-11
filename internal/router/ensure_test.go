package router_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

func TestRouterServiceEnsure(t *testing.T) {
	t.Run("writes the stack, starts it once, and leaves nothing for a second start to change", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		raw, err := os.ReadFile(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Services map[string]struct {
				Image       string   `json:"image"`
				Ports       []string `json:"ports"`
				Command     []string `json:"command"`
				NetworkMode string   `json:"network_mode"`
			} `json:"services"`
			Networks map[string]any `json:"networks"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Services) != 2 || document.Services["router"].Image == "" || document.Services["index"].Image == "" {
			t.Fatalf("services = %+v", document.Services)
		}
		if document.Networks != nil {
			t.Fatalf("the stack declares networks: %v", document.Networks)
		}
		if got := document.Services["router"]; got.NetworkMode != "host" || len(got.Ports) != 0 {
			t.Fatalf("router = %+v, want host network and no published ports", got)
		}
		if !containsSubstring(document.Services["router"].Command, "--entrypoints.web.address=127.0.0.1:80") {
			t.Fatalf("router command = %v", document.Services["router"].Command)
		}
		if got := document.Services["index"].Ports; len(got) != 1 || got[0] != "127.0.0.1:"+strconv.Itoa(seedIndexPort)+":8080" {
			t.Fatalf("index ports = %v", got)
		}
		if info, err := os.Stat(w.State.IndexFile); err != nil || info.IsDir() {
			t.Fatalf("expected a page to exist before the health check needs one: %v", err)
		}
		// What the containers read must be readable by root with no capabilities; what they don't, stays private.
		if got := fileMode(t, w.State.DynamicDir); got != 0o755 {
			t.Errorf("DynamicDir mode = %o, want 0755", got)
		}
		if got := fileMode(t, w.State.IndexDir); got != 0o755 {
			t.Errorf("IndexDir mode = %o, want 0755", got)
		}
		if got := fileMode(t, w.State.IndexFile); got != 0o644 {
			t.Errorf("IndexFile mode = %o, want 0644", got)
		}
		if got := fileMode(t, w.State.ComposeFile); got != 0o600 {
			t.Errorf("ComposeFile mode = %o, want 0600", got)
		}

		ups := w.ups()
		if len(ups) != 1 {
			t.Fatalf("expected exactly one `up`, got %v", ups)
		}
		want := []string{
			"compose", "--project-directory", w.State.StateDir, "--project-name", testProject, "-f", w.State.ComposeFile,
			"up", "--detach", "--wait", "--remove-orphans",
		}
		if strings.Join(ups[0], "\x00") != strings.Join(want, "\x00") {
			t.Errorf("up args = %v, want %v", ups[0], want)
		}
		lines, _ := w.Logger.Snapshot()
		if got := linesWithPrefix(lines, "warn:"); len(got) != 0 {
			t.Errorf("expected no warnings, got %v", got)
		}

		// A second Ensure of a running router is idempotent: Compose is
		// asked again and decides, and the stack file is byte for byte the same.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		again, err := os.ReadFile(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(raw) {
			t.Error("the stack file changed between two Ensures of an unchanged router")
		}
		if got := len(w.ups()); got != 2 {
			t.Errorf("`up` calls = %d, want 2", got)
		}
	})

	t.Run("runs Compose with the same guarded environment as worktrees, so a shell setting cannot split the two", func(t *testing.T) {
		// COMPOSE_IGNORE_ORPHANS would make `up --remove-orphans` a hard error.
		w := newWorldOpts(t, "", 80, func(o *router.Options) {
			o.Environ = []string{"PATH=/usr/bin", "COMPOSE_IGNORE_ORPHANS=true", "COMPOSE_FILE=/elsewhere/compose.yaml"}
		})
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		env := w.firstUp().env
		for _, unwanted := range []string{"COMPOSE_IGNORE_ORPHANS", "COMPOSE_FILE"} {
			for _, entry := range env {
				if strings.HasPrefix(entry, unwanted+"=") {
					t.Errorf("expected %s to be stripped, got %v", unwanted, env)
				}
			}
		}
		if !containsSubstring(env, "COMPOSE_PROGRESS=plain") {
			t.Errorf("expected COMPOSE_PROGRESS=plain, got %v", env)
		}
		if !containsSubstring(env, "PATH=/usr/bin") {
			t.Errorf("expected PATH to be inherited, got %v", env)
		}
	})

	t.Run("builds the stack in the mode the override names, whatever the daemon looks like", func(t *testing.T) {
		w := newWorldOpts(t, "", 8080, func(o *router.Options) { o.ModeOverride = "bridge" })
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"127.0.0.1:8080:80"`) || strings.Contains(string(raw), `"network_mode"`) {
			t.Fatalf("not a bridge-mode stack:\n%s", raw)
		}
	})

	t.Run("refuses a router name that could rewrite the project", func(t *testing.T) {
		for _, name := range []string{"bad name", "x/../y", "-leading"} {
			if _, err := router.New(router.Options{ProjectName: name}); err == nil {
				t.Errorf("expected %q to be refused", name)
			}
		}
	})

	t.Run("refuses a router whose protocol label is not a protocol", func(t *testing.T) {
		w := newWorld(t, 80, "")
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Labels[testNS+".router-protocol"] = "0x10"
		w.Fake.setDoc("router-1", doc)
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "invalid protocol label") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("started over an unreadable protocol")
		}
	})

	t.Run("refuses to downgrade a newer router that is running", func(t *testing.T) {
		w := newWorld(t, 80, "")
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol + 1)
		w.Fake.setDoc("router-1", doc)
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Upgrade rather than downgrading") || !strings.Contains(err.Error(), "'duke router down'") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("recreated a newer router")
		}
	})

	t.Run("refuses a running router of an older protocol, or with none, until it is stopped", func(t *testing.T) {
		for name, protocol := range map[string]string{"older": strconv.Itoa(router.Protocol - 1), "unlabelled": ""} {
			w := newWorld(t, 80, "")
			doc := routerDoc(router.ModeHost, 80)
			if protocol == "" {
				delete(doc.Config.Labels, testNS+".router-protocol")
			} else {
				doc.Config.Labels[testNS+".router-protocol"] = protocol
			}
			w.Fake.setDoc("router-1", doc)
			err := w.Router.Ensure(context.Background())
			if err == nil || !strings.Contains(err.Error(), "Run 'duke router down' first, then run this again.") {
				t.Fatalf("%s: got %v", name, err)
			}
			if len(w.ups()) != 0 {
				t.Fatalf("%s: started over a router it does not share a layout with", name)
			}
		}
	})

	t.Run("replaces a stopped router of any protocol rather than refusing it: it serves nobody", func(t *testing.T) {
		for _, protocol := range []int{router.Protocol - 1, router.Protocol + 1} {
			w := newWorld(t, 80, "")
			doc := routerDoc(router.ModeHost, 80)
			doc.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(protocol)
			doc.State.Status = "exited"
			w.Fake.setDoc("router-1", doc)
			if err := w.Router.Ensure(context.Background()); err != nil {
				t.Fatalf("protocol %d: %v", protocol, err)
			}
		}
	})

	t.Run("refuses a router it cannot inspect rather than recreating it blind", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.deleteDoc("router-1")
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "could not be inspected") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("recreated a router it could not inspect")
		}
	})

	t.Run("refuses a router that carries no berth-root label, the same as one belonging to another root", func(t *testing.T) {
		w := newWorld(t, 80, "")
		doc := routerDoc(router.ModeHost, 80)
		delete(doc.Config.Labels, router.RouterHomeLabel)
		w.Fake.setDoc("router-1", doc)
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not owned by this berth root") || !strings.Contains(err.Error(), "'duke router down'") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("recreated a router it does not own")
		}
	})

	t.Run("refuses a router that belongs to a different berth root, leaving it untouched", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "/home/b/.berth" })
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Labels[router.RouterHomeLabel] = "/home/a/.berth"
		w.Fake.setDoc("router-1", doc)
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "belongs to a different berth root (/home/a/.berth)") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("recreated a router it does not own")
		}
	})

	t.Run("refuses a router whose berth-root label is empty when this one's own home is not", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "/home/b/.berth" })
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "belongs to a different berth root ()") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("takes over a stopped router whatever root it belonged to: it serves nobody", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "/home/b/.berth" })
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Labels[router.RouterHomeLabel] = "/home/a/.berth"
		doc.State.Status = "exited"
		w.Fake.setDoc("router-1", doc)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("a stopped router must be takeable: %v", err)
		}
	})

	t.Run("hands the router to a new berth root: 'router down' then 'router up' adopts it cleanly", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-b" })
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Labels[router.RouterHomeLabel] = "home-a"
		w.Fake.setDoc("router-1", doc)
		if err := w.Router.Ensure(context.Background()); err == nil {
			t.Fatal("expected the foreign router to be refused")
		}
		if err := w.Router.Down(context.Background()); err != nil {
			t.Fatal(err)
		}
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected the new root to adopt it: %v", err)
		}
	})
}

func TestEnsureDetectsTheMode(t *testing.T) {
	t.Run("surfaces a refused daemon before anything is written or started", func(t *testing.T) {
		for name, tc := range map[string]struct {
			setup func(*dockerFake)
			want  string
		}{
			"rootless": {func(d *dockerFake) {
				d.info = `{"Name":"box","OperatingSystem":"Ubuntu","OSType":"linux","SecurityOptions":["name=seccomp","name=rootless"]}`
			}, "#121"},
			"remote": {func(d *dockerFake) { d.endpoint = "ssh://dev@build-host" }, "ssh://dev@build-host"},
		} {
			w := newWorld(t, 80, "")
			tc.setup(w.Fake)
			stopped(t, w, router.ModeHost)
			err := w.Router.Ensure(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: got %v", name, err)
			}
			if _, statErr := os.Stat(w.State.ComposeFile); statErr == nil {
				t.Errorf("%s: a stack file was written", name)
			}
			if len(w.ups()) != 0 {
				t.Errorf("%s: Compose was asked to start", name)
			}
		}
	})

	t.Run("asks the current context for the endpoint unless DOCKER_HOST was passed in", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.DockerHost = "tcp://10.1.2.3:2375" })
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "tcp://10.1.2.3:2375") {
			t.Fatalf("got %v", err)
		}
		for _, c := range w.Fake.snapshotCalls() {
			if len(c.args) > 1 && c.args[0] == "context" {
				t.Fatalf("asked the context although DOCKER_HOST was given: %v", c.args)
			}
		}
	})

	t.Run("fails closed when the endpoint cannot be read", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.endpointExitCode = 1
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Could not tell which Docker endpoint duke is talking to") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("reports a daemon that does not answer `docker info` as Docker being unavailable", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.infoExitCode = 1
		w.Fake.infoStderr = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Docker is not available") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestEnsureChecksThePortsAreFree(t *testing.T) {
	t.Run("names the container publishing a port the router needs", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[80] = true
		w.Fake.publishers["80"] = []string{"other-proxy-1"}
		err := w.Router.Ensure(context.Background())
		if err == nil ||
			!strings.Contains(err.Error(), "127.0.0.1:80 (the router) is held by container other-proxy-1") ||
			!strings.Contains(err.Error(), "choose another port with BERTH_ROUTER_PORT or [router] port in settings.toml") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatal("started over a held port")
		}
	})

	t.Run("names another process, and how to find it, when no container accounts for the port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[8080] = true
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "held by another process (find it with: ss -ltnp 'sport = :8080')") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("suggests lsof on a client that is not Linux", func(t *testing.T) {
		w := newWorldOpts(t, "", 8080, func(o *router.Options) { o.ClientOS = "darwin"; o.ModeOverride = "bridge" })
		stopped(t, w, router.ModeBridge)
		w.Probe.Busy[8080] = true
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "lsof -nP -iTCP:8080 -sTCP:LISTEN") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("checks the index and ping ports too, and says how to move them", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[seedIndexPort] = true
		w.Probe.Busy[seedPingPort] = true
		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected a refusal")
		}
		for _, want := range []string{
			"127.0.0.1:" + strconv.Itoa(seedIndexPort) + " (the checkout index) is held by",
			"127.0.0.1:" + strconv.Itoa(seedPingPort) + " (the router's ping) is held by",
			"remove " + w.State.StateFile + " so the next start picks another port",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
	})

	t.Run("does not check a ping port in bridge mode, which has none to bind", func(t *testing.T) {
		w := newWorldOpts(t, "", 8080, func(o *router.Options) { o.ModeOverride = "bridge" })
		stopped(t, w, router.ModeBridge)
		w.Probe.Busy[seedPingPort] = true
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("does not mistake the ports its own running containers hold for a conflict", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Probe.Busy[80] = true
		w.Probe.Busy[seedIndexPort] = true
		w.Probe.Busy[seedPingPort] = true
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("its own ports reported as conflicts: %v", err)
		}
	})

	t.Run("still checks a new port when the running router is on another one", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		old := routerDoc(router.ModeHost, 80)
		w.Fake.setDoc("router-1", old)
		w.Probe.Busy[8080] = true
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:8080 (the router)") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("treats a router that is not running as holding nothing", func(t *testing.T) {
		w := newWorld(t, 80, "")
		doc := routerDoc(router.ModeHost, 80)
		doc.State.Status = "restarting"
		w.Fake.setDoc("router-1", doc)
		w.Probe.Busy[80] = true
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:80 (the router)") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("probes with the operating system by default, dialling a port something listens on", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback: %v", err)
		}
		defer func() { _ = listener.Close() }()
		port := listener.Addr().(*net.TCPAddr).Port
		w := newWorldOpts(t, "", port, func(o *router.Options) { o.Probe = nil })
		stopped(t, w, router.ModeHost)
		err = w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(port)+" (the router) is held by") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("only dials a privileged port: an unprivileged user cannot bind-probe one", func(t *testing.T) {
		if conn, err := net.Dial("tcp4", "127.0.0.1:80"); err == nil {
			_ = conn.Close()
			t.Skip("something listens on 127.0.0.1:80 here")
		}
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Probe = nil })
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("a free privileged port was refused: %v", err)
		}
	})
}

func TestEnsureExplainsAFailedStart(t *testing.T) {
	t.Run("explains a failed start in terms of the port, with the containers' last log lines", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "Error response from daemon: Bind for 127.0.0.1:8080 failed: port is already allocated"
		w.Fake.logs = "router-1  | level=error msg=\"listen tcp 127.0.0.1:8080: bind: address already in use\"\n"

		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected the start to fail")
		}
		for _, want := range []string{
			"127.0.0.1:8080 is free, or choose another port with BERTH_ROUTER_PORT",
			"port is already allocated",
			"Last log lines:\nrouter-1  | level=error",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
		var logged bool
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "logs") {
				logged = containsArg(c.args, "--tail") && containsArg(c.args, "20")
			}
		}
		if !logged {
			t.Error("the logs were not asked for with --tail 20")
		}
	})

	t.Run("names the settings file beside BERTH_ROUTER_PORT, or generically without a path", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.SettingsPath = "/berth/settings.toml" })
		w.Fake.upExitCode = 1
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "or [router] port in /berth/settings.toml.") {
			t.Fatalf("got %v", err)
		}

		w = newWorld(t, 8080, "")
		w.Fake.upExitCode = 1
		err = w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "or [router] port in settings.toml.") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("leaves out the log lines when none can be read", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.upExitCode = 1
		err := w.Router.Ensure(context.Background())
		if err == nil || strings.Contains(err.Error(), "Last log lines") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("does not blame the port when the socket is not ours", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"

		err := w.Router.Ensure(context.Background())
		want := "Docker is not available: permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
	})
}

func TestEnsureAllocatesTheRouterStatePorts(t *testing.T) {
	// freshWorld is a world with no state file yet and a stopped router.
	freshWorld := func(t *testing.T, port int, extra func(*router.Options)) *testWorld {
		t.Helper()
		w := newWorldOpts(t, "", port, extra)
		if err := os.Remove(w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		stopped(t, w, router.ModeHost)
		return w
	}

	t.Run("picks the index and ping ports once, inside the range, and keeps them", func(t *testing.T) {
		w := freshWorld(t, 80, nil)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		first, found, err := router.ReadState(w.State)
		if err != nil || !found {
			t.Fatalf("ReadState = %+v, %v, %v", first, found, err)
		}
		if !registryRangeContains(first.IndexPort) || !registryRangeContains(first.PingPort) || first.IndexPort == first.PingPort {
			t.Fatalf("state = %+v, want two distinct ports in the test range", first)
		}
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		second, _, err := router.ReadState(w.State)
		if err != nil || second != first {
			t.Fatalf("state changed between Ensures: %+v then %+v (%v)", first, second, err)
		}
		if got := fileMode(t, w.State.StateFile); got != 0o600 {
			t.Errorf("state file mode = %o, want 0600", got)
		}
		raw, err := os.ReadFile(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "127.0.0.1:"+strconv.Itoa(first.IndexPort)+":8080") ||
			!strings.Contains(string(raw), "--entrypoints.traefik.address=127.0.0.1:"+strconv.Itoa(first.PingPort)) {
			t.Errorf("the stack does not use the allocated ports:\n%s", raw)
		}
		fragment, err := os.ReadFile(router.IndexFragmentPath(w.State))
		if err != nil || !strings.Contains(string(fragment), router.TargetURL(first.IndexPort)) {
			t.Errorf("the index route does not target the index port: %s (%v)", fragment, err)
		}
	})

	t.Run("allocates no ping port in bridge mode, and adds one when the mode becomes host", func(t *testing.T) {
		w := freshWorld(t, 8080, func(o *router.Options) { o.ModeOverride = "bridge" })
		stopped(t, w, router.ModeBridge)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridge, _, err := router.ReadState(w.State)
		if err != nil || bridge.IndexPort == 0 || bridge.PingPort != 0 {
			t.Fatalf("state = %+v, %v; want an index port and no ping port", bridge, err)
		}

		hostRouter, err := router.New(router.Options{
			State: w.State, Port: 8080, Runner: w.Fake, Logger: w.Logger, ControlHome: w.ControlHome,
			PortRange: registrytestRange(), Probe: w.Probe, ClientOS: "linux",
		})
		if err != nil {
			t.Fatal(err)
		}
		stopped(t, w, router.ModeHost)
		if err := hostRouter.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		host, _, err := router.ReadState(w.State)
		if err != nil || host.IndexPort != bridge.IndexPort || host.PingPort == 0 {
			t.Fatalf("state = %+v, %v; want the same index port and a ping port", host, err)
		}
	})

	t.Run("skips the ports registrations hold and the router's own port", func(t *testing.T) {
		start := registrytestRange().Low + startOf("router", "index")
		w := freshWorld(t, start, nil)
		writeRegistration(t, w.ControlHome, regRepoA, regWtA, map[string]int{"api": start + 1})
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, _, err := router.ReadState(w.State)
		if err != nil {
			t.Fatal(err)
		}
		if state.IndexPort == start || state.PingPort == start || state.IndexPort == start+1 || state.PingPort == start+1 {
			t.Fatalf("state = %+v collides with the router port %d or a registration's %d", state, start, start+1)
		}
	})

	t.Run("replaces a stored port that has become the router's own, or the other state port", func(t *testing.T) {
		w := newWorld(t, seedIndexPort, "")
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, _, err := router.ReadState(w.State)
		if err != nil || state.IndexPort == seedIndexPort || state.IndexPort == 0 {
			t.Fatalf("state = %+v, %v; want a new index port", state, err)
		}
		if state.PingPort != seedPingPort {
			t.Errorf("ping port = %d, want it kept (%d)", state.PingPort, seedPingPort)
		}
	})

	t.Run("refuses to allocate while a registration cannot be read, since it may hold a port", func(t *testing.T) {
		w := freshWorld(t, 80, nil)
		writeRegistration(t, w.ControlHome, regRepoA, regWtA, nil)
		corruptRegistration(t, w.ControlHome, regRepoA, regWtA)
		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if _, statErr := os.Stat(w.State.StateFile); statErr == nil {
			t.Error("a state file was written anyway")
		}
	})

	t.Run("reports an exhausted range", func(t *testing.T) {
		w := freshWorld(t, 80, func(o *router.Options) { o.PortRange = tinyRange() })
		for port := tinyRange().Low; port <= tinyRange().High; port++ {
			w.Probe.Busy[port] = true
		}
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "No free port for the checkout index") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestEnsureRepairsRouteFiles(t *testing.T) {
	// withRegistrations is a world whose Router reads regs, plus a route file
	// for (regRepoA, regWtA, api) pointing at port.
	withRegistrations := func(t *testing.T, regs *fakeRegistrations, port int) (*testWorld, string) {
		t.Helper()
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Registrations = regs })
		doc := router.NewFragmentDocument("p-api", "Host(`api.x.localhost`)", port)
		if err := router.WriteFragment(w.State, regRepoA, regWtA, "api", doc); err != nil {
			t.Fatal(err)
		}
		return w, router.FragmentPath(w.State, regRepoA, regWtA, "api")
	}
	registered := func(port int) router.Registration {
		return router.Registration{Status: router.Registered, Ports: map[string]int{"api": port}}
	}

	t.Run("leaves a route file whose worktree has no record here: it may be another control home's", func(t *testing.T) {
		regs := newFakeRegistrations(router.Registration{Status: router.Unregistered})
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("an unmatched route file was removed: %v", err)
		}
	})

	t.Run("removes a route file whose registration holds no port for its service", func(t *testing.T) {
		regs := newFakeRegistrations(router.Registration{Status: router.Registered, Ports: map[string]int{"web": 30011}})
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatal("the portless route file survived")
		}
		lines, _ := w.Logger.Snapshot()
		if !containsSubstring(lines, "Repaired a route file: api: its registration records no port for the service") {
			t.Errorf("repair not reported: %v", lines)
		}
	})

	t.Run("points a route file at the port its registration records", func(t *testing.T) {
		regs := newFakeRegistrations(registered(30020))
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		doc, err := router.ReadFragment(w.State, path)
		if err != nil || doc.TargetPort() != 30020 {
			t.Fatalf("target = %d (%v), want 30020", doc.TargetPort(), err)
		}
	})

	t.Run("leaves a file of a repository this control home does not know", func(t *testing.T) {
		regs := newFakeRegistrations(router.Registration{Status: router.UnknownRepository})
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("a file that may belong to another control home was removed: %v", err)
		}
	})

	t.Run("reads the record again before removing, so a just-registered worktree keeps its file", func(t *testing.T) {
		regs := newFakeRegistrations(router.Registration{Status: router.Registered, Ports: map[string]int{"web": 30011}}, registered(30010))
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the file was removed on a stale read: %v", err)
		}
		if regs.lookups() < 2 {
			t.Fatalf("lookups = %d, want a second read before acting", regs.lookups())
		}
	})

	t.Run("warns, and still succeeds, when a record cannot be read", func(t *testing.T) {
		regs := &fakeRegistrations{err: os.ErrPermission}
		w, path := withRegistrations(t, regs, 30010)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("a repair problem failed the start: %v", err)
		}
		lines, _ := w.Logger.Snapshot()
		if !containsSubstring(linesWithPrefix(lines, "warn:"), "Could not reconcile route files") {
			t.Errorf("no warning: %v", lines)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("the file went despite the unreadable record")
		}
	})
}
