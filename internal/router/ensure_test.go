package router_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/router"
)

func TestRouterServiceEnsure(t *testing.T) {
	t.Run("writes the stack, starts it once, verifies it, and leaves a correct router alone", func(t *testing.T) {
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
				Image   string   `json:"image"`
				Ports   []string `json:"ports"`
				Command []string `json:"command"`
			} `json:"services"`
			Networks map[string]struct {
				Name     string `json:"name"`
				Internal bool   `json:"internal"`
			} `json:"networks"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		var names []string
		for name := range document.Services {
			names = append(names, name)
		}
		wantServices := map[string]bool{"index": true, "router": true}
		if len(names) != len(wantServices) {
			t.Fatalf("services = %v", names)
		}
		for _, name := range names {
			if !wantServices[name] {
				t.Fatalf("unexpected service %q", name)
			}
		}
		if info, err := os.Stat(w.State.IndexFile); err != nil || info.IsDir() {
			t.Fatalf("expected a page to exist before the health check needs one: %v", err)
		}
		if got := document.Services["router"].Ports; len(got) != 1 || got[0] != "127.0.0.1:80:80" {
			t.Fatalf("router ports = %v", got)
		}
		if !containsSubstring(document.Services["router"].Command, "--providers.file.directory="+router.DynamicConfigMount) {
			t.Fatalf("router command = %v", document.Services["router"].Command)
		}
		if document.Networks["router"].Name != testNetwork {
			t.Fatalf("networks = %+v", document.Networks)
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

	t.Run("recreates a drifted router once, and accepts it when the recreation fixes it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		drifted := routerDoc(w.State.DynamicDir, w.Port)
		drifted.HostConfig.ReadonlyRootfs = false
		w.Fake.setDoc("router-1", drifted)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		ups := w.ups()
		if len(ups) != 2 || containsArg(ups[0], "--force-recreate") || !containsArg(ups[1], "--force-recreate") {
			t.Fatalf("ups = %v", ups)
		}
		lines, _ := w.Logger.Snapshot()
		if !containsSubstring(lines, "warn: The shared router is not configured as expected (router: root filesystem is writable)") {
			t.Errorf("expected a drift warning, got %v", lines)
		}
	})

	t.Run("fails, listing every problem, when recreation does not help", func(t *testing.T) {
		w := newWorld(t, 80, "")
		bad := routerDoc(w.State.DynamicDir, w.Port)
		bad.HostConfig.CapDrop = nil
		bad.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "80"}}}
		w.Fake.setDoc("router-1", bad)

		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(),
			"could not be brought to its expected configuration:\n  router: capabilities are not dropped\n  router: port 80 is not published only on 127.0.0.1:80") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 2 {
			t.Fatalf("ups = %v", w.ups())
		}
	})

	t.Run("ignores a stack file it cannot parse, which it is about to replace anyway", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w)
		if err := os.WriteFile(w.State.ComposeFile, []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		// One corrupt byte there must not brick every `up` on the machine:
		// write() overwrites it unconditionally rather than reading it first.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ignores a stack file it cannot read either", func(t *testing.T) {
		skipIfRoot(t)
		w := newWorld(t, 80, "")
		taken(t, w)
		if err := os.WriteFile(w.State.ComposeFile, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.State.ComposeFile, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(w.State.ComposeFile, 0o600)

		// Unreadable is no answer here either: write() never reads it, only replaces it.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lets a container carrying no protocol label through, refusing nothing", func(t *testing.T) {
		w := newWorld(t, 80, "")
		unlabelled := routerDoc(w.State.DynamicDir, w.Port)
		rest := testLabels("router")
		delete(rest, testNS+".router-protocol")
		unlabelled.Config.Labels = rest
		w.Fake.setDoc("router-1", unlabelled)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		// Not ours, or started under another namespace: no answer, not an invalid one.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a router name that could rewrite Traefik's discovery predicate", func(t *testing.T) {
		base := router.Options{Port: 80, Runner: newDockerFake(), Logger: logger.Silent{}}
		bad := base
		bad.ProjectName = "berth-router`,`x"
		if _, err := router.New(bad); err == nil || !strings.Contains(err.Error(), "Invalid router project name") {
			t.Errorf("got %v", err)
		}
		badNetwork := base
		badNetwork.Network = "n`,`x"
		if _, err := router.New(badNetwork); err == nil || !strings.Contains(err.Error(), "Invalid router network name") {
			t.Errorf("got %v", err)
		}
		ok := base
		ok.ProjectName = "berth-router-2.test_a"
		if _, err := router.New(ok); err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("refuses a router whose protocol label is not a protocol", func(t *testing.T) {
		w := newWorld(t, 80, "")
		odd := routerDoc(w.State.DynamicDir, w.Port)
		odd.Config.Labels = testLabels("router")
		odd.Config.Labels[testNS+".router-protocol"] = "1e3"
		w.Fake.setDoc("router-1", odd)

		err := w.Router.Ensure(context.Background())
		want := "The shared router carries an invalid protocol label: 1e3. Remove the router with 'duke router down' and run this again."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	})

	t.Run("refuses to downgrade a newer router that is running", func(t *testing.T) {
		w := newWorld(t, 80, "")
		newer := routerDoc(w.State.DynamicDir, w.Port)
		newer.Config.Labels = testLabels("router")
		newer.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol + 1)
		w.Fake.setDoc("router-1", newer)

		err := w.Router.Ensure(context.Background())
		want := "The shared router is at protocol " + strconv.Itoa(router.Protocol+1) + "; this version speaks " + strconv.Itoa(router.Protocol) + "."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v", err)
		}
		if !strings.HasSuffix(strings.TrimRight(err.Error(), "\n"), "remove it with 'duke router down'.") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Fatalf("expected the refusal to precede every mutation, got %v", w.ups())
		}
	})

	t.Run("tells Traefik to discover routes only from the file provider, never from Docker, and labels the index the old way", func(t *testing.T) {
		// Pinned as literal text: the verifier compares against the same constants, so it alone cannot catch a typo here.
		for _, want := range router.RouterCommand() {
			if strings.Contains(want, "--providers.docker") {
				t.Errorf("RouterCommand still names a Docker-provider flag: %q", want)
			}
		}
		if !containsSubstring(router.RouterCommand(), "--providers.file.directory="+router.DynamicConfigMount) {
			t.Errorf("RouterCommand missing the file-provider directory")
		}
		if !containsSubstring(router.RouterCommand(), "--providers.file.watch=true") {
			t.Errorf("RouterCommand missing the file-provider watch flag")
		}
		labels := router.IndexRouteLabels("n").Map()
		if labels["dev.berth.router"] != "n" || labels["traefik.enable"] != "true" {
			t.Errorf("IndexRouteLabels = %v", labels)
		}
	})

	t.Run("refuses a router it cannot inspect rather than recreating it blind", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.clearDocs() // the containers are there; `docker inspect` says nothing about them

		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "The shared router exists but could not be inspected.") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 { // a router this version may not downgrade is never recreated blind
			t.Errorf("expected no `up` calls, got %v", w.ups())
		}
	})

	t.Run("refuses a router that carries no berth-root label, the same as one belonging to another root", func(t *testing.T) {
		w := newWorld(t, 80, "")
		unlabelled := routerDoc(w.State.DynamicDir, w.Port)
		rest := testLabels("router")
		delete(rest, router.RouterHomeLabel)
		unlabelled.Config.Labels = rest
		w.Fake.setDoc("router-1", unlabelled)

		err := w.Router.Ensure(context.Background())
		want := "The shared router is not owned by this berth root. Hand it to this one: stop it with 'duke router down', then run this again."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
		if ups := w.ups(); len(ups) != 0 {
			t.Fatalf("expected no `compose up` at all, got %v", ups)
		}
	})

	t.Run("refuses a router whose berth-root label is empty, naming the empty root, when this one's own home is not also empty", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-a" })
		unowned := routerDoc(w.State.DynamicDir, w.Port)
		unowned.Config.Labels = testLabels("router")
		unowned.Config.Labels[router.RouterHomeLabel] = ""
		w.Fake.setDoc("router-1", unowned)

		err := w.Router.Ensure(context.Background())
		want := "The shared router belongs to a different berth root (). Hand it to this one: stop it with 'duke router down', then run this again."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	})

	t.Run("treats an empty berth-root label as owned when this router's own home is also empty", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "" })
		unowned := routerDoc(w.State.DynamicDir, w.Port)
		unowned.Config.Labels = testLabels("router")
		unowned.Config.Labels[router.RouterHomeLabel] = ""
		w.Fake.setDoc("router-1", unowned)

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected an equally homeless router to be treated as this one's own, got %v", err)
		}
	})

	t.Run("takes over a stopped router whatever root it belonged to: it serves nobody", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-a" })
		stopped := routerDoc(w.State.DynamicDir, w.Port)
		stopped.Config.Labels = testLabels("router")
		stopped.Config.Labels[router.RouterHomeLabel] = "home-b"
		stopped.State.Status = "exited"
		w.Fake.setDoc("router-1", stopped)
		w.Fake.mu.Lock()
		w.Fake.onUp = w.setHealthy // the start brings it back, this root's own
		w.Fake.mu.Unlock()

		owner, err := w.Router.Owner(context.Background())
		if err != nil || owner.Running {
			t.Fatalf("Owner = %+v, %v; want not running", owner, err)
		}
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected a stopped foreign router to be taken over, got %v", err)
		}
	})

	t.Run("refuses a router it cannot inspect, rather than reading no answer as no label", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-a" })
		w.Fake.clearDocs() // the containers are there; `docker inspect` says nothing about them

		if _, err := w.Router.Owner(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router exists but could not be inspected.") {
			t.Fatalf("Owner: got %v", err)
		}
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "The shared router exists but could not be inspected.") {
			t.Fatalf("Ensure: got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Errorf("expected no `up` calls, got %v", w.ups())
		}
	})

	t.Run("refuses a router that belongs to a different berth root, leaving it untouched", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-a" })
		foreign := routerDoc(w.State.DynamicDir, w.Port)
		foreign.Config.Labels = testLabels("router")
		foreign.Config.Labels[router.RouterHomeLabel] = "home-b"
		w.Fake.setDoc("router-1", foreign)

		err := w.Router.Ensure(context.Background())
		want := "The shared router belongs to a different berth root (home-b). Hand it to this one: stop it with 'duke router down', then run this again."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
		if ups := w.ups(); len(ups) != 0 {
			t.Fatalf("expected no `compose up` at all, got %v", ups)
		}
	})

	t.Run("hands the router to a new berth root: 'router down' then 'router up' adopts it cleanly", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.Home = "home-a" })
		foreign := routerDoc(w.State.DynamicDir, w.Port)
		foreign.Config.Labels = testLabels("router")
		foreign.Config.Labels[router.RouterHomeLabel] = "home-b"
		w.Fake.setDoc("router-1", foreign)

		if err := w.Router.Ensure(context.Background()); err == nil {
			t.Fatal("expected the first Ensure, against another berth root's router, to refuse")
		}

		// 'router down' stops the router unconditionally, whoever owns it —
		// the explicit way to hand it over (#82) — never gated on ownership
		// itself.
		if err := w.Router.Down(context.Background()); err != nil {
			t.Fatal(err)
		}
		// The fake's own `down` does not remove containers the way a real
		// daemon's does; taken() stages exactly what a real `down` leaves.
		taken(t, w)
		// taken()'s own onUp stages the package's default fixture, labelled
		// with an empty home; this world's own home is "home-a", so the
		// next `up` is staged by hand instead, to carry the label a real
		// `compose up` against this world's own options would actually
		// stamp.
		w.Fake.mu.Lock()
		w.Fake.onUp = func() {
			w.Fake.mu.Lock()
			w.Fake.ids = map[string]string{"router": "router-1", "index": "index-1"}
			w.Fake.mu.Unlock()
			adopted := routerDoc(w.State.DynamicDir, w.Port)
			adopted.Config.Labels = testLabels("router")
			adopted.Config.Labels[router.RouterHomeLabel] = "home-a"
			w.Fake.setDoc("router-1", adopted)
			w.Fake.setDoc("index-1", indexDoc(w.State.IndexDir))
		}
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected 'router up' from the new berth root to adopt cleanly: %v", err)
		}
	})

	t.Run("explains a failed start in terms of the port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "Error response from daemon: Bind for 127.0.0.1:8080 failed: port is already allocated"

		err := w.Router.Ensure(context.Background())
		if err == nil ||
			!strings.Contains(err.Error(), "127.0.0.1:8080 is free, or choose another port with BERTH_ROUTER_PORT") ||
			!strings.Contains(err.Error(), "port is already allocated") {
			t.Fatalf("got %v", err)
		}
	})
}
