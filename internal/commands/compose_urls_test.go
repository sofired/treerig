package commands_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// dashURL is what a host route "dash" on subdomain "dash" of the feature-login
// worktree answers on, behind a router on port 30080.
const dashURL = "http://dash.feature-login.acme.localhost:30080"

// addDashRoute records a host route "dash" (port 5173, subdomain "dash") in
// ref's registration, keeping the rest of the record.
func addDashRoute(t *testing.T, w *commandstest.World, ref worktree.Ref) {
	t.Helper()
	record, err := registry.NewStore(w.ControlHome, ref.RepositoryID).Find(ref.WorktreeID)
	if err != nil || record == nil {
		t.Fatalf("no registration for %s: %v", ref.Slug, err)
	}
	if _, err := registrytest.WriteRecord(w.ControlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
		Slug: record.Slug, Domain: record.Domain, LabelNamespace: record.LabelNamespace,
		Initialization: "complete", Ports: record.Ports,
		HostRoutes: []registrytest.HostRouteJSON{{Name: "dash", Port: 5173, Subdomain: "dash"}},
	}); err != nil {
		t.Fatal(err)
	}
}

// requireDash fails the test unless every environment in envs carries the
// host route's URL as TREERIG_SERVICE_URL_DASH, behind router port 30080, and
// there is at least one.
func requireDash(t *testing.T, what string, envs []map[string]string) {
	t.Helper()
	if len(envs) == 0 {
		t.Fatalf("no %s call was recorded", what)
	}
	for _, env := range envs {
		if env["TREERIG_SERVICE_URL_DASH"] != dashURL {
			t.Errorf("%s: TREERIG_SERVICE_URL_DASH = %q, want %q", what, env["TREERIG_SERVICE_URL_DASH"], dashURL)
		}
	}
}

// composeEnvOf is the environment of every recorded `docker compose` call
// whose argument list contains verb, as name-to-value maps, in call order.
func composeEnvOf(w *commandstest.World, verb string) []map[string]string {
	var envs []map[string]string
	for _, spec := range w.Runner.Calls() {
		if spec.Command != "docker" || !slices.Contains(spec.Args, verb) {
			continue
		}
		envs = append(envs, envMap(spec))
	}
	return envs
}

// worktreeComposeEnvOf is composeEnvOf without the calls about the shared
// router's own project, which carry no worktree's URLs: `status` asks it
// whether its container is up.
func worktreeComposeEnvOf(w *commandstest.World, verb string) []map[string]string {
	var envs []map[string]string
	for _, spec := range w.Runner.Calls() {
		if spec.Command != "docker" || !slices.Contains(spec.Args, verb) || slices.Contains(spec.Args, router.DefaultName) {
			continue
		}
		envs = append(envs, envMap(spec))
	}
	return envs
}

// envMap decodes spec's Env into a map.
func envMap(spec run.Spec) map[string]string {
	env := map[string]string{}
	for _, entry := range spec.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return env
}

func TestComposeSeesTheRoutedURLs(t *testing.T) {
	t.Run("up hands every Compose call the URLs `status` shows, router port included (#177)", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Environ = []string{"TREERIG_URL=http://stale", "TREERIG_SERVICE_URL_GONE=http://stale"}

		result, err := up(t, tc, commandstest.NewFakeRouter(30080), &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		want := map[string]string{}
		for _, entry := range result.URLs {
			want["TREERIG_SERVICE_URL_"+strings.ToUpper(entry.Service)] = entry.URL
			if entry.Service == "api" {
				want["TREERIG_URL"] = entry.URL
			}
		}
		if want["TREERIG_URL"] != "http://feature-login.acme.localhost:30080" ||
			want["TREERIG_SERVICE_URL_MAIL"] != "http://mail.feature-login.acme.localhost:30080" {
			t.Fatalf("test setup: urls = %+v", result.URLs)
		}
		for _, verb := range []string{"config", "up"} {
			envs := composeEnvOf(w, verb)
			if len(envs) == 0 {
				t.Fatalf("no compose %s call was recorded", verb)
			}
			for _, env := range envs {
				for name, value := range want {
					if env[name] != value {
						t.Errorf("compose %s: %s = %q, want %q", verb, name, env[name], value)
					}
				}
				if _, stale := env["TREERIG_SERVICE_URL_GONE"]; stale {
					t.Errorf("compose %s: an inherited TREERIG_SERVICE_URL_GONE survived", verb)
				}
			}
		}
	})

	t.Run("every config read of one up sees identical values, and a second up sees the same again", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		for range 2 {
			if _, err := up(t, tc, commandstest.NewFakeRouter(30080), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
				t.Fatalf("Up: %v", err)
			}
		}
		var first map[string]string
		for _, env := range append(composeEnvOf(w, "config"), composeEnvOf(w, "up")...) {
			urls := map[string]string{}
			for name, value := range env {
				if name == "TREERIG_URL" || strings.HasPrefix(name, "TREERIG_SERVICE_URL_") {
					urls[name] = value
				}
			}
			if len(urls) == 0 {
				t.Fatal("a compose call had no URL variables")
			}
			if first == nil {
				first = urls
			}
			if !mapsEqual(first, urls) {
				t.Errorf("values differ between Compose calls: %v vs %v", first, urls)
			}
		}
	})

	t.Run("up leaves them unset, and drops inherited ones, when the router's port is unresolved", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Environ = []string{"TREERIG_URL=http://stale"}
		if _, err := up(t, tc, commandstest.NewFakeRouter(0), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("Up: %v", err)
		}
		for _, verb := range []string{"config", "up"} {
			for _, env := range composeEnvOf(w, verb) {
				for name := range env {
					if name == "TREERIG_URL" || strings.HasPrefix(name, "TREERIG_SERVICE_URL_") {
						t.Errorf("compose %s carries %s with no router port", verb, name)
					}
				}
			}
		}
	})

	t.Run("down hands Compose the same values `up` did", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		// `down` has nothing to run Compose for unless `up` left something.
		w.Docker.Realistic()
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.RouterPort = 30080
		if _, err := up(t, tc, commandstest.NewFakeRouter(30080), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("Up: %v", err)
		}
		if err := commands.DownWithContext(context.Background(), tc.CommandContext); err != nil {
			t.Fatalf("Down: %v", err)
		}
		downs := composeEnvOf(w, "down")
		if len(downs) == 0 {
			t.Fatal("no compose down call was recorded")
		}
		for _, env := range downs {
			if env["TREERIG_URL"] != "http://feature-login.acme.localhost:30080" ||
				env["TREERIG_SERVICE_URL_MAIL"] != "http://mail.feature-login.acme.localhost:30080" {
				t.Errorf("compose down env: TREERIG_URL = %q, MAIL = %q", env["TREERIG_URL"], env["TREERIG_SERVICE_URL_MAIL"])
			}
		}
	})
}

// mapsEqual reports whether a and b hold the same entries.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestEveryVerbHandsComposeTheRoutedURLs(t *testing.T) {
	t.Run("status, host routes included", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", "dev.treerig", "api")
		addDashRoute(t, w, ref)
		tc := w.Context("", acme, nil)
		tc.RouterPort = 30080
		if _, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatalf("status: %v", err)
		}
		requireDash(t, "compose ps", worktreeComposeEnvOf(w, "ps"))
	})

	t.Run("backup (restore shares runBundleHook): the Compose call and the hook see the same URLs, host routes included", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		addDashRoute(t, bw.World, bw.ref)
		bw.tc.RouterPort = 30080
		dir := t.TempDir() + "/bundle"
		if err := commands.BackupWithContext(context.Background(), bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatalf("backup: %v", err)
		}
		requireDash(t, "compose ps", composeEnvOf(bw.World, "ps"))
		hooks := bw.started()
		if len(hooks) != 1 {
			t.Fatalf("started %d hooks, want 1", len(hooks))
		}
		if hook := envMap(hooks[0]); hook["TREERIG_SERVICE_URL_DASH"] != dashURL || hook["TREERIG_URL"] != "http://feature-login.acme.localhost:30080" {
			t.Errorf("backup hook sees TREERIG_SERVICE_URL_DASH = %q, TREERIG_URL = %q", hook["TREERIG_SERVICE_URL_DASH"], hook["TREERIG_URL"])
		}
	})

	t.Run("up's shared-use config read, before the lock", func(t *testing.T) {
		sw := newSharedWorld(t)
		sw.router = commandstest.NewFakeRouter(30080)
		tc := sw.context("")
		sw.mustUp(t, tc)
		shared := sw.project(t, tc)
		var reads []map[string]string
		for _, spec := range sw.Runner.Calls() {
			if spec.Command == "docker" && slices.Contains(spec.Args, "config") && slices.Contains(spec.Args, "--profile") &&
				!slices.Contains(spec.Args, shared) {
				reads = append(reads, envMap(spec))
			}
		}
		if len(reads) == 0 {
			t.Fatal("no shared-use config read was recorded")
		}
		for _, env := range reads {
			if env["TREERIG_URL"] != "http://feature-login.acme.localhost:30080" {
				t.Errorf("shared-use config read: TREERIG_URL = %q", env["TREERIG_URL"])
			}
		}
	})

	t.Run("dev's log follower", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		// dev ends when the fake follower returns; its error is not what this subtest checks.
		if err := commands.DevWithContext(context.Background(), tc.CommandContext, commandstest.NewFakeRouter(30080), &commandstest.FakeIndex{}); err != nil {
			t.Logf("dev: %v", err)
		}
		logs := composeEnvOf(w, "logs")
		if len(logs) == 0 {
			t.Fatal("no compose logs call was recorded")
		}
		for _, env := range logs {
			if env["TREERIG_URL"] != "http://feature-login.acme.localhost:30080" {
				t.Errorf("compose logs: TREERIG_URL = %q", env["TREERIG_URL"])
			}
		}
	})
}
