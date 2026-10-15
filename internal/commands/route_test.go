package commands_test

// Host routes (#6, docs/design/routing.md, decision 11): `route add`, `route
// rm` and `route ls`, and what `up`, `down`, `worktree remove`, `urls` and
// `run` do with the routes they leave behind.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
)

// fakeListeners answers a host route's listener probe from a fixed table of
// "host:port" addresses, so no test binds a socket.
type fakeListeners map[string]bool

// Answers is ports.ListenerProbe's Answers.
func (f fakeListeners) Answers(_ context.Context, host string, port int) bool {
	return f[net.JoinHostPort(host, strconv.Itoa(port))]
}

// listeningOn is a fakeListeners whose given addresses answer.
func listeningOn(addresses ...string) fakeListeners {
	f := fakeListeners{}
	for _, address := range addresses {
		f[address] = true
	}
	return f
}

// fakeModes is a router mode source with a fixed answer.
type fakeModes struct {
	choice router.ModeChoice
	err    error
	asked  int
}

// ModeChoice is routeModes' ModeChoice.
func (f *fakeModes) ModeChoice(context.Context) (router.ModeChoice, error) {
	f.asked++
	return f.choice, f.err
}

// routeWorld is a portsWorld whose worktree is registered and holds its
// services' ports.
type routeWorld struct {
	*portsWorld
	ref registry.RegisteredWorktree
}

// newRouteWorld registers feature/login with api and mail pinned to loopback
// ports, as a finished `up` leaves it, but without running one.
func newRouteWorld(t *testing.T) *routeWorld {
	t.Helper()
	pw := newPortsWorld(t)
	pw.tc.Listeners = listeningOn("127.0.0.1:5173")
	ref := pw.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
	pw.SetPorts(ref, map[string]int{"api": 30010, "mail": 30011})
	return &routeWorld{portsWorld: pw, ref: *pw.record(t)}
}

// newUpRouteWorld is a routeWorld brought up by `up` itself, so the service
// route files exist and the card is written.
func newUpRouteWorld(t *testing.T) *routeWorld {
	t.Helper()
	rw := newRouteWorld(t)
	if _, err := rw.up(t, commands.UpOptions{}); err != nil {
		t.Fatalf("up: %v", err)
	}
	// What `up` said is not what the test is about.
	rw.tc.Logger.Lines, rw.tc.Logger.Printed = nil, nil
	rw.ref = *rw.record(t)
	return rw
}

// record is the worktree's registration as it stands.
func (pw *portsWorld) record(t *testing.T) *registry.RegisteredWorktree {
	t.Helper()
	current, err := pw.tc.Registry.Current(context.Background())
	if err != nil || current == nil {
		t.Fatalf("Current: %v, %v", current, err)
	}
	return &current.RegisteredWorktree
}

// add runs `route add`.
func (rw *routeWorld) add(options commands.RouteAddOptions) error {
	return commands.RouteAddWithContext(context.Background(), rw.tc.CommandContext, nil, options)
}

// mustAdd runs `route add` and fails the test on error.
func (rw *routeWorld) mustAdd(t *testing.T, options commands.RouteAddOptions) {
	t.Helper()
	if err := rw.add(options); err != nil {
		t.Fatalf("route add %s: %v", options.Name, err)
	}
}

// file is where the route called name keeps its route file.
func (rw *routeWorld) file(name string) string {
	return router.HostFragmentPath(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, name)
}

// routes are the host routes the record holds.
func (rw *routeWorld) routes(t *testing.T) []registry.HostRoute {
	t.Helper()
	return rw.record(t).HostRoutes
}

// hasFile reports whether path exists.
func hasFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// requireRefusal fails unless err is a refusal matching pattern.
func requireRefusal(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one matching %q", pattern)
	}
	if !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("err = %q, want it to match %q", err, pattern)
	}
}

// strPtr is &s, for a route's optional path.
func strPtr(s string) *string { return &s }

func TestRouteAdd(t *testing.T) {
	t.Run("records the route, writes its route file and says where it routes", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})

		want := []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app"}}
		if got := rw.routes(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("recorded %+v, want %+v", got, want)
		}
		doc, err := router.ReadFragment(rw.RouterState, rw.file("web"))
		if err != nil {
			t.Fatal(err)
		}
		name := layout.ComposeProjectName(rw.ref.RepositoryID, rw.ref.WorktreeID) + "_host_web"
		entry, ok := doc.HTTP.Routers[name]
		if !ok || entry.Rule != "Host(`app.feature-login.acme.localhost`)" || entry.Service != name || doc.TargetPort() != 5173 {
			t.Fatalf("route file = %+v", doc)
		}
		lines, _ := rw.tc.Logger.Snapshot()
		if got := strings.Join(lines, "\n"); !strings.Contains(got, "Routed http://app.feature-login.acme.localhost to 127.0.0.1:5173 (host route web).") {
			t.Fatalf("logged %q", got)
		}
	})

	t.Run("gives a path the same rule a service's path gets", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "api-v1", Port: 5173, Subdomain: "api", Path: strPtr("/v1")})
		doc, err := router.ReadFragment(rw.RouterState, rw.file("api-v1"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range doc.HTTP.Routers {
			if entry.Rule != "Host(`api.feature-login.acme.localhost`) && PathPrefix(`/v1`)" {
				t.Fatalf("rule = %q", entry.Rule)
			}
		}
	})

	t.Run("folds the router's port into the printed URL", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.tc.RouterPort = 8080
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		lines, _ := rw.tc.Logger.Snapshot()
		if got := strings.Join(lines, "\n"); !strings.Contains(got, "Routed http://app.feature-login.acme.localhost:8080 to") {
			t.Fatalf("logged %q", got)
		}
	})

	t.Run("two routes may name one port, at different places", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		rw.mustAdd(t, commands.RouteAddOptions{Name: "ui", Port: 5173, Subdomain: "ui"})
		if len(rw.routes(t)) != 2 {
			t.Fatalf("routes = %+v", rw.routes(t))
		}
	})

	t.Run("works from a router whose state directory does not exist yet", func(t *testing.T) {
		rw := newRouteWorld(t)
		if err := os.RemoveAll(rw.RouterState.StateDir); err != nil {
			t.Fatal(err)
		}
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		if !hasFile(rw.file("web")) {
			t.Fatal("no route file")
		}
	})

	t.Run("refuses what cannot be a route, naming the reason, and writes nothing", func(t *testing.T) {
		cases := []struct {
			name    string
			options commands.RouteAddOptions
			pattern string
		}{
			{"an uppercase name", commands.RouteAddOptions{Name: "Web", Port: 5173, Subdomain: "app"}, `The route name "Web" must be lower-case letters, digits, and hyphens\.`},
			{"a name over 63 characters", commands.RouteAddOptions{Name: strings.Repeat("a", 64), Port: 5173, Subdomain: "app"}, `must be at most 63 characters`},
			{"port 0", commands.RouteAddOptions{Name: "web", Port: 0, Subdomain: "app"}, `--port must be a port between 1 and 65535, not 0\.`},
			{"port 70000", commands.RouteAddOptions{Name: "web", Port: 70000, Subdomain: "app"}, `not 70000`},
			{"a subdomain with a dot", commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "a.b"}, `--subdomain must be lower-case letters, digits, and hyphens, not starting or ending with a hyphen\.`},
			{"a path without a slash", commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app", Path: strPtr("v1")}, `--path must be a URL path starting with /`},
			{"a path with a backtick", commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app", Path: strPtr("/a`b")}, `--path must be a URL path`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rw := newRouteWorld(t)
				requireRefusal(t, rw.add(c.options), c.pattern)
				if len(rw.routes(t)) != 0 || hasFile(rw.file(c.options.Name)) {
					t.Fatalf("something was written: %+v", rw.routes(t))
				}
			})
		}
	})

	t.Run("refuses what clashes with the worktree, and leaves it as it was", func(t *testing.T) {
		cases := []struct {
			name    string
			prepare func(rw *routeWorld)
			options commands.RouteAddOptions
			pattern string
		}{
			{"a service's name", nil, commands.RouteAddOptions{Name: "api", Port: 5173, Subdomain: "app"},
				`api is a service in treerig\.toml; a host route needs a name of its own\.`},
			{"a shared service's name", func(rw *routeWorld) {
				rw.tc.Loaded.Config.Shared = &config.Shared{File: "shared.yaml", Service: []config.SharedService{{Name: "mailpit"}}}
			}, commands.RouteAddOptions{Name: "mailpit", Port: 5173, Subdomain: "app"},
				`mailpit is a shared service in treerig\.toml; a host route needs a name of its own`},
			{"another host route's name", func(rw *routeWorld) {
				rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
			}, commands.RouteAddOptions{Name: "web", Port: 5174, Subdomain: "other"},
				`There is already a host route called web\. Remove it with 'treerig route rm web' first\.`},
			{"the host root a service answers at", nil, commands.RouteAddOptions{Name: "web", Port: 5173},
				`Service api already answers at http://feature-login\.acme\.localhost\.`},
			{"a service's subdomain", nil, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "mail"},
				`Service mail already answers at http://mail\.feature-login\.acme\.localhost\.`},
			{"another host route's place", func(rw *routeWorld) {
				rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app", Path: strPtr("/x")})
			}, commands.RouteAddOptions{Name: "ui", Port: 5174, Subdomain: "app", Path: strPtr("/x")},
				`Host route web already answers at http://app\.feature-login\.acme\.localhost/x\.`},
			{"the router's own port", nil, commands.RouteAddOptions{Name: "web", Port: 80, Subdomain: "app"},
				`Port 80 is the router's own, so a route to it would loop\.`},
			{"a port pinned for a service", func(rw *routeWorld) {
				rw.SetPorts(rw.portsWorld.ref(t), map[string]int{"api": 30010})
			}, commands.RouteAddOptions{Name: "web", Port: 30010, Subdomain: "app"},
				`Port 30010 is the pinned loopback port of service api of feature-login\.acme\.localhost\.`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rw := newRouteWorld(t)
				if c.prepare != nil {
					c.prepare(rw)
				}
				before := rw.routes(t)
				requireRefusal(t, rw.add(c.options), c.pattern)
				if after := rw.routes(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("routes went from %+v to %+v", before, after)
				}
				if len(before) == 0 && hasFile(rw.file(c.options.Name)) {
					t.Fatal("a route file was written")
				}
			})
		}
	})

	t.Run("adding an identical route writes a missing route file again and changes no record", func(t *testing.T) {
		rw := hostRouteWorld(t)
		before, err := os.ReadFile(rw.file("web"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rw.file("web")); err != nil {
			t.Fatal(err)
		}
		recordBefore, _ := os.ReadFile(registry.NewStore(rw.ControlHome, rw.ref.RepositoryID).RecordPath(rw.ref.WorktreeID))
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		after, err := os.ReadFile(rw.file("web"))
		if err != nil || string(before) != string(after) {
			t.Fatalf("the healed file differs: %v\n%s\n%s", err, before, after)
		}
		recordAfter, _ := os.ReadFile(registry.NewStore(rw.ControlHome, rw.ref.RepositoryID).RecordPath(rw.ref.WorktreeID))
		if string(recordBefore) != string(recordAfter) {
			t.Fatal("the heal changed the record")
		}
		lines, _ := rw.tc.Logger.Snapshot()
		if got := strings.Join(lines, "\n"); !strings.Contains(got, "Host route web was already recorded; wrote its route file again") {
			t.Fatalf("logged %q", got)
		}
	})

	t.Run("a route with a recorded name and any other field is still refused", func(t *testing.T) {
		rw := hostRouteWorld(t)
		requireRefusal(t, rw.add(commands.RouteAddOptions{Name: "web", Port: 5174, Subdomain: "app"}), `There is already a host route called web`)
		requireRefusal(t, rw.add(commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "other"}), `There is already a host route called web`)
	})

	t.Run("needs a registered worktree", func(t *testing.T) {
		pw := newPortsWorld(t)
		err := commands.RouteAddWithContext(context.Background(), pw.tc.CommandContext, nil, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		requireRefusal(t, err, `This worktree is not registered\. Run 'treerig up' first\.`)
		if entries, _ := os.ReadDir(pw.RouterState.DynamicDir); len(entries) != 0 {
			t.Fatalf("route files written for an unregistered worktree: %v", entries)
		}
	})

	t.Run("takes the worktree's operation lock, and gives the record up if the wait is cancelled", func(t *testing.T) {
		rw := newRouteWorld(t)
		scope, _ := lock.WorktreeScopes(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID, rw.ref.Slug, procid.Markers{})
		held := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := held.Acquire(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Release() }()

		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		err := commands.RouteAddWithContext(ctx, rw.tc.CommandContext, nil, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		if err == nil {
			t.Fatal("route add went ahead under a held operation lock")
		}
		if len(rw.routes(t)) != 0 || hasFile(rw.file("web")) {
			t.Fatal("something was written without the lock")
		}
	})

	t.Run("refuses inside its own worktree's hooks, where its caller holds the lock", func(t *testing.T) {
		rw := newRouteWorld(t)
		inside(rw.tc, rw.ref.WorktreeID)
		requireNestedRefusal(t, rw.add(commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"}), "feature-login.acme.localhost")
	})

	t.Run("takes the record back when the route file cannot be written", func(t *testing.T) {
		rw := newRouteWorld(t)
		// A regular file where the dynamic directory belongs.
		if err := os.RemoveAll(rw.RouterState.StateDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rw.RouterState.StateDir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, rw.add(commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"}), `Could not write the route file of "web":`)
		if got := rw.routes(t); len(got) != 0 {
			t.Fatalf("the record kept %+v", got)
		}
	})

	t.Run("refuses to write through a symbolic link planted as the dynamic directory", func(t *testing.T) {
		rw := newRouteWorld(t)
		outside := t.TempDir()
		if err := os.MkdirAll(rw.RouterState.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(rw.RouterState.DynamicDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, rw.RouterState.DynamicDir); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, rw.add(commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"}), `Could not write the route file of "web"`)
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("wrote into the link's target: %v", entries)
		}
		if len(rw.routes(t)) != 0 {
			t.Fatal("the record kept the route")
		}
	})
}

func TestRouteAddSaysWhatThePortAnswersOn(t *testing.T) {
	bridge := router.ModeChoice{Mode: router.ModeBridge}
	host := router.ModeChoice{Mode: router.ModeHost}
	cases := []struct {
		name      string
		listeners fakeListeners
		modes     *fakeModes
		warn      string
		info      string
		askedMode bool
	}{
		{"nothing to say when 127.0.0.1 answers", listeningOn("127.0.0.1:5173"), &fakeModes{choice: bridge}, "", "", false},
		{"nothing to say when both answer", listeningOn("127.0.0.1:5173", "[::1]:5173"), &fakeModes{choice: bridge}, "", "", false},
		{"warns to bind 127.0.0.1 when only ::1 answers and the proxy is in bridge mode", listeningOn("[::1]:5173"), &fakeModes{choice: bridge},
			"Port 5173 answers on ::1 but not on 127.0.0.1. The router cannot reach ::1 on Docker Desktop and OrbStack. Bind the process to 127.0.0.1 instead", "", true},
		{"says nothing when only ::1 answers and the proxy is in host mode, which reaches it", listeningOn("[::1]:5173"), &fakeModes{choice: host}, "", "", true},
		{"warns when only ::1 answers and the mode cannot be read", listeningOn("[::1]:5173"), &fakeModes{err: clierr.New("no docker")},
			"its mode could not be read here. Bind the process to 127.0.0.1", "", true},
		{"warns when only ::1 answers and there is no router to ask", listeningOn("[::1]:5173"), nil,
			"Bind the process to 127.0.0.1 instead", "", false},
		{"says the route answers 502 until something listens", listeningOn(), &fakeModes{choice: bridge}, "",
			"Nothing is listening on 127.0.0.1:5173 yet; requests get a 502 until a process is.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rw := newRouteWorld(t)
			rw.tc.Listeners = c.listeners
			var modes interface {
				ModeChoice(context.Context) (router.ModeChoice, error)
			}
			if c.modes != nil {
				modes = c.modes
			}
			err := commands.RouteAddWithContext(context.Background(), rw.tc.CommandContext, modes, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
			if err != nil {
				t.Fatal(err)
			}
			logged, _ := rw.tc.Logger.Snapshot()
			all := strings.Join(logged, "\n")
			if c.warn == "" && strings.Contains(all, "answers on ::1") {
				t.Errorf("unexpected warning in %q", all)
			}
			if c.warn != "" && !strings.Contains(all, "warn: Port 5173 answers on ::1") || !strings.Contains(all, c.warn) {
				t.Errorf("want a warning containing %q, logged %q", c.warn, all)
			}
			if c.info != "" && !strings.Contains(all, c.info) {
				t.Errorf("want %q, logged %q", c.info, all)
			}
			if c.info == "" && strings.Contains(all, "Nothing is listening") {
				t.Errorf("unexpected silence note in %q", all)
			}
			if c.modes != nil && (c.modes.asked > 0) != c.askedMode {
				t.Errorf("mode asked %d times, want asked = %v", c.modes.asked, c.askedMode)
			}
		})
	}
}

func TestRouteRm(t *testing.T) {
	ctx := context.Background()

	t.Run("removes the route file and the record's entry and keeps the other routes", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		rw.mustAdd(t, commands.RouteAddOptions{Name: "ui", Port: 5174, Subdomain: "ui"})
		if err := commands.RouteRmWithContext(ctx, rw.tc.CommandContext, "web"); err != nil {
			t.Fatal(err)
		}
		if got := rw.routes(t); len(got) != 1 || got[0].Name != "ui" {
			t.Fatalf("routes = %+v", got)
		}
		if hasFile(rw.file("web")) || !hasFile(rw.file("ui")) {
			t.Fatalf("files: web %v, ui %v", hasFile(rw.file("web")), hasFile(rw.file("ui")))
		}
		lines, _ := rw.tc.Logger.Snapshot()
		if got := strings.Join(lines, "\n"); !strings.Contains(got, "Removed host route web (http://app.feature-login.acme.localhost).") {
			t.Fatalf("logged %q", got)
		}
	})

	t.Run("finishes a removal whose route file is already gone", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		if err := os.Remove(rw.file("web")); err != nil {
			t.Fatal(err)
		}
		if err := commands.RouteRmWithContext(ctx, rw.tc.CommandContext, "web"); err != nil {
			t.Fatal(err)
		}
		if len(rw.routes(t)) != 0 {
			t.Fatal("the record kept the route")
		}
	})

	t.Run("a name that is no host route is refused, and a service's file is not touched", func(t *testing.T) {
		rw := newRouteWorld(t)
		serviceFile := router.FragmentPath(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "api")
		if err := router.WriteFragment(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "api", router.NewFragmentDocument("x", "Host(`x`)", 30010)); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, commands.RouteRmWithContext(ctx, rw.tc.CommandContext, "api"), `There is no host route called "api"\. 'treerig route ls' lists them\.`)
		if !hasFile(serviceFile) {
			t.Fatal("a service's route file was removed")
		}
	})

	t.Run("needs a registered worktree", func(t *testing.T) {
		pw := newPortsWorld(t)
		requireRefusal(t, commands.RouteRmWithContext(ctx, pw.tc.CommandContext, "web"), `This worktree is not registered`)
	})

	t.Run("takes the operation lock", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		scope, _ := lock.WorktreeScopes(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID, rw.ref.Slug, procid.Markers{})
		held := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := held.Acquire(ctx, "test"); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Release() }()
		short, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		defer cancel()
		if err := commands.RouteRmWithContext(short, rw.tc.CommandContext, "web"); err == nil {
			t.Fatal("route rm went ahead under a held operation lock")
		}
		if len(rw.routes(t)) != 1 || !hasFile(rw.file("web")) {
			t.Fatal("the route was removed without the lock")
		}
	})

	t.Run("refuses inside its own worktree's hooks", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		inside(rw.tc, rw.ref.WorktreeID)
		requireNestedRefusal(t, commands.RouteRmWithContext(ctx, rw.tc.CommandContext, "web"), "feature-login.acme.localhost")
	})
}

func TestRouteLs(t *testing.T) {
	ctx := context.Background()

	t.Run("lists the routes by name, with the URL and the target", func(t *testing.T) {
		rw := newRouteWorld(t)
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		rw.mustAdd(t, commands.RouteAddOptions{Name: "api-v1", Port: 8080, Subdomain: "api", Path: strPtr("/v1")})
		if err := commands.RouteLsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		want := "api-v1  http://api.feature-login.acme.localhost/v1  127.0.0.1:8080\n" +
			"web     http://app.feature-login.acme.localhost     127.0.0.1:5173"
		if got := printedText(rw.tc); got != want {
			t.Fatalf("printed:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("prints a JSON array, empty as []", func(t *testing.T) {
		rw := newRouteWorld(t)
		if err := commands.RouteLsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		if got := printedText(rw.tc); got != "[]" {
			t.Fatalf("printed %q", got)
		}
		rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
		rw.tc.Logger.Printed = nil
		if err := commands.RouteLsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		var entries []commands.RouteReportEntry
		if err := json.Unmarshal([]byte(printedText(rw.tc)), &entries); err != nil {
			t.Fatal(err)
		}
		want := []commands.RouteReportEntry{{Name: "web", URL: "http://app.feature-login.acme.localhost", Port: 5173, Subdomain: "app"}}
		if !reflect.DeepEqual(entries, want) {
			t.Fatalf("entries = %+v", entries)
		}
	})

	t.Run("says so on stderr with --json too, and prints []", func(t *testing.T) {
		rw := newRouteWorld(t)
		if err := commands.RouteLsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		lines, printed := rw.tc.Logger.Snapshot()
		if len(printed) != 1 || printed[0] != "[]" || !strings.Contains(strings.Join(lines, "\n"), "No host routes.") {
			t.Fatalf("lines %v, printed %v", lines, printed)
		}
	})

	t.Run("says so on stderr when there are none, and prints nothing", func(t *testing.T) {
		rw := newRouteWorld(t)
		if err := commands.RouteLsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, printed := rw.tc.Logger.Snapshot()
		if len(printed) != 0 || !strings.Contains(strings.Join(lines, "\n"), "No host routes.") {
			t.Fatalf("lines %v, printed %v", lines, printed)
		}
	})

	t.Run("takes no lock", func(t *testing.T) {
		rw := newRouteWorld(t)
		scope, _ := lock.WorktreeScopes(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID, rw.ref.Slug, procid.Markers{})
		held := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := held.Acquire(ctx, "test"); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Release() }()
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := commands.RouteLsWithContext(short, rw.tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatalf("route ls waited on the lock: %v", err)
		}
	})
}

// hostRouteWorld is a routeWorld with a route "web" at app.<host> on port 5173.
func hostRouteWorld(t *testing.T) *routeWorld {
	t.Helper()
	rw := newUpRouteWorld(t)
	rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
	return rw
}

func TestListHostRouteNames(t *testing.T) {
	rw := newRouteWorld(t)
	rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
	rw.mustAdd(t, commands.RouteAddOptions{Name: "api-v1", Port: 8080, Subdomain: "api"})
	names, err := commands.ListHostRouteNamesWithContext(context.Background(), rw.tc.CommandContext)
	if err != nil {
		t.Fatal(err)
	}
	want := []commands.HostRouteName{{Name: "api-v1", Port: 8080}, {Name: "web", Port: 5173}}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %+v", names)
	}
}

func TestHostRoutesAcrossTheLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("up leaves a host route and its file alone, and lists it with the service URLs", func(t *testing.T) {
		rw := hostRouteWorld(t)
		before, err := os.ReadFile(rw.file("web"))
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(rw.file("web"))
		result, err := rw.up(t, commands.UpOptions{})
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(rw.file("web"))
		if err != nil || string(before) != string(after) {
			t.Fatalf("up changed the route file: %v\n%s\n%s", err, before, after)
		}
		if again, _ := os.Stat(rw.file("web")); !again.ModTime().Equal(info.ModTime()) {
			t.Error("up rewrote the route file")
		}
		if got := rw.routes(t); len(got) != 1 || got[0].Name != "web" {
			t.Fatalf("routes after up = %+v", got)
		}
		urls := map[string]string{}
		for _, entry := range result.URLs {
			urls[entry.Service] = entry.URL
		}
		if urls["web"] != "http://app.feature-login.acme.localhost" || urls["api"] == "" {
			t.Fatalf("URLs = %+v", result.URLs)
		}
		if got := printedText(rw.tc); !strings.Contains(got, "web   http://app.feature-login.acme.localhost") {
			t.Errorf("up printed %q", got)
		}
	})

	t.Run("up gives hooks the host route's URL and port beside the services'", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
		var mu sync.Mutex
		var hookEnv []string
		fallback := rw.Runner.Handler
		rw.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.HasSuffix(spec.Command, "/hooks/prepare") {
				mu.Lock()
				hookEnv = spec.Env
				mu.Unlock()
				return run.Result{}, nil
			}
			return fallback(ctx, spec)
		}
		if _, err := rw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !containsEnv(hookEnv, "TREERIG_SERVICE_URL_WEB", "http://app.feature-login.acme.localhost") ||
			!containsEnv(hookEnv, "TREERIG_SERVICE_PORT_WEB", "5173") ||
			!containsEnv(hookEnv, "TREERIG_SERVICE_URL_MAIL", "http://mail.feature-login.acme.localhost") {
			t.Errorf("hook env = %v", hookEnv)
		}
	})

	t.Run("up does not put a host route on the checkout index card, where it would count as a stopped service", func(t *testing.T) {
		rw := hostRouteWorld(t)
		if _, err := rw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(card.IndexCardPath(rw.ControlHome, rw.ref.RepositoryID, rw.ref.WorktreeID))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "web") {
			t.Fatalf("the card names the host route:\n%s", data)
		}
	})

	t.Run("down removes the service route files and keeps the host route's file and record", func(t *testing.T) {
		rw := hostRouteWorld(t)
		serviceFile := router.FragmentPath(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "api")
		if !hasFile(serviceFile) {
			t.Fatal("setup: no service file")
		}
		if err := commands.DownWithContext(ctx, rw.tc.CommandContext); err != nil {
			t.Fatal(err)
		}
		if hasFile(serviceFile) {
			t.Error("down kept the service's route file")
		}
		if !hasFile(rw.file("web")) {
			t.Error("down removed the host route's file")
		}
		if got := rw.routes(t); len(got) != 1 {
			t.Errorf("down changed the record: %+v", got)
		}
	})

	t.Run("worktree remove stops, with the record and the resources intact, when the host route files cannot be removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		rw := hostRouteWorld(t)
		// No service route files, so the host route file is the first thing
		// removal fails on.
		if err := router.RemoveWorktreeFragments(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rw.RouterState.DynamicDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(rw.RouterState.DynamicDir, 0o755) })
		before := rw.Docker.Count("rm ")
		_, err := commands.RemoveWithContext(ctx, rw.tc.CommandContext, &commands.Env{CommandName: "treerig"}, approved(commands.RemoveOptions{}))
		requireRefusal(t, err, `Could not remove this worktree's host route files`)
		if found, _ := rw.tc.Registry.Store(rw.ref.RepositoryID).Find(rw.ref.WorktreeID); found == nil || len(found.HostRoutes) != 1 {
			t.Fatalf("the record did not survive: %+v", found)
		}
		if rw.Docker.Count("rm ") != before {
			t.Fatal("something was reaped")
		}
		if !hasFile(rw.file("web")) {
			t.Fatal("the host route file went")
		}
	})

	t.Run("worktree remove removes the host route files before the record, and only this worktree's", func(t *testing.T) {
		rw := hostRouteWorld(t)
		otherRepo, otherWt := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
		if err := router.WriteHostFragment(rw.RouterState, otherRepo, otherWt, "web", router.HostFragmentDocument(otherRepo, otherWt, "web", "Host(`x`)", 1)); err != nil {
			t.Fatal(err)
		}
		// A file the record never named, as a crashed `route add` could leave.
		if err := router.WriteHostFragment(rw.RouterState, rw.ref.RepositoryID, rw.ref.WorktreeID, "forgotten", router.HostFragmentDocument(rw.ref.RepositoryID, rw.ref.WorktreeID, "forgotten", "Host(`y`)", 2)); err != nil {
			t.Fatal(err)
		}
		if _, err := commands.RemoveWithContext(ctx, rw.tc.CommandContext, &commands.Env{CommandName: "treerig"}, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if hasFile(rw.file("web")) || hasFile(rw.file("forgotten")) {
			t.Error("a host route file of the removed worktree is left")
		}
		if !hasFile(router.HostFragmentPath(rw.RouterState, otherRepo, otherWt, "web")) {
			t.Error("another worktree's host route file was removed")
		}
		if found, _ := rw.tc.Registry.Store(rw.ref.RepositoryID).Find(rw.ref.WorktreeID); found != nil {
			t.Errorf("the record is still there: %+v", found)
		}
	})

	t.Run("urls lists the host route after the services, with its port", func(t *testing.T) {
		rw := hostRouteWorld(t)
		urls, err := commands.UrlsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(urls) != 3 || urls[2].Service != "web" || urls[2].URL != "http://app.feature-login.acme.localhost" || urls[2].Port != 5173 || urls[2].Label != nil {
			t.Fatalf("urls = %+v", urls)
		}
	})

	t.Run("urls --json carries the host route like a service", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Logger.Printed = nil
		if _, err := commands.UrlsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		var entries []commands.URLReportEntry
		if err := json.Unmarshal([]byte(printedText(rw.tc)), &entries); err != nil {
			t.Fatal(err)
		}
		last := entries[len(entries)-1]
		if last.Service != "web" || last.URL != "http://app.feature-login.acme.localhost" || last.Port != 5173 {
			t.Fatalf("entries = %+v", entries)
		}
	})

	t.Run("urls --json marks only the host route", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Logger.Printed = nil
		if _, err := commands.UrlsWithContext(ctx, rw.tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		var entries []commands.URLReportEntry
		if err := json.Unmarshal([]byte(printedText(rw.tc)), &entries); err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.HostRoute != (entry.Service == "web") {
				t.Errorf("entry %+v", entry)
			}
		}
		if !strings.Contains(printedText(rw.tc), `"hostRoute": true`) {
			t.Errorf("printed %s", printedText(rw.tc))
		}
	})

	t.Run("run gives a command the host route's URL and port", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Environ = []string{"PATH=/usr/bin"}
		rw.tc.Loaded.Config.Commands = config.Commands{{Name: "show", Command: config.Command{Command: "./show", KillAfter: "10s"}}}
		var mu sync.Mutex
		var env []string
		fallback := rw.Runner.Handler
		rw.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Output == run.Interactive {
				mu.Lock()
				env = spec.Env
				mu.Unlock()
				return run.Result{}, nil
			}
			return fallback(ctx, spec)
		}
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "show"}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !containsEnv(env, "TREERIG_SERVICE_URL_WEB", "http://app.feature-login.acme.localhost") || !containsEnv(env, "TREERIG_SERVICE_PORT_WEB", "5173") {
			t.Errorf("env = %v", env)
		}
	})

	t.Run("a host route survives a re-domain of the worktree and its file follows the new hostname", func(t *testing.T) {
		rw := hostRouteWorld(t)
		moved := rw.Context("", "moved.localhost", nil)
		moved.RouterState = rw.RouterState
		givePorts(moved)
		moved.PortRange, moved.PortProbe = rw.tc.PortRange, rw.tc.PortProbe
		if _, err := up(t, moved, rw.router, rw.index, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		routes := rw.routes(t)
		if len(routes) != 1 || routes[0].Name != "web" {
			t.Fatalf("routes after a re-domain = %+v", routes)
		}
		// The hostname changed, so the file is stale until the router's repair
		// rewrites it, which `up` runs through router.Ensure (not in this test).
		regs := commands.ControlHomeRegistrations(rw.ControlHome)
		registration, err := regs.Lookup(rw.ref.RepositoryID, rw.ref.WorktreeID)
		if err != nil {
			t.Fatal(err)
		}
		if target := registration.HostRoutes["web"]; target.Rule != "Host(`app.feature-login.moved.localhost`)" || target.Port != 5173 {
			t.Fatalf("repair would write %+v", target)
		}
		problems, err := router.FindRouteProblems(rw.RouterState, regs)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, problem := range problems {
			if problem.Fragment.HostRoute && problem.Fragment.Service == "web" && problem.Kind == router.RouteStaleHostRoute {
				found = true
			}
		}
		if !found {
			t.Fatalf("repair would not notice the stale host route file: %+v", problems)
		}
		repaired, err := router.RepairRoutes(ctx, rw.RouterState, regs)
		if err != nil {
			t.Fatal(err)
		}
		if len(repaired) == 0 {
			t.Fatal("nothing repaired")
		}
		doc, err := router.ReadFragment(rw.RouterState, rw.file("web"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range doc.HTTP.Routers {
			if entry.Rule != "Host(`app.feature-login.moved.localhost`)" {
				t.Fatalf("rule after repair = %q", entry.Rule)
			}
		}
	})
}

func TestUpRefusesWhatClashesWithAHostRoute(t *testing.T) {
	t.Run("a service named like a host route", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Loaded.Config.Service = append(rw.tc.Loaded.Config.Service, config.Service{Name: "web", Subdomain: "elsewhere"})
		before := len(rw.Docker.Calls())
		_, err := rw.up(t, commands.UpOptions{})
		requireRefusal(t, err, `Service web has the name of a host route of this worktree\. Remove the route with 'treerig route rm web', or rename the service\.`)
		if calls := rw.Docker.Calls()[before:]; len(calls) != 0 {
			t.Fatalf("Docker was called after the refusal: %v", calls)
		}
	})

	t.Run("a service answering where a host route does", func(t *testing.T) {
		rw := hostRouteWorld(t)
		rw.tc.Loaded.Config.Service = append(rw.tc.Loaded.Config.Service, config.Service{Name: "docs", Subdomain: "app"})
		_, err := rw.up(t, commands.UpOptions{})
		requireRefusal(t, err, `Service docs answers where the host route web does, so both would claim one hostname\. Remove the route with 'treerig route rm web', or change the service's subdomain or path\.`)
	})

	t.Run("under the operation lock too: a route added after the early check is still refused", func(t *testing.T) {
		rw := newRouteWorld(t)
		// The route is added, and the service declared, while up waits for the
		// router: after the first check, before the lock.
		rw.router.BeforeEnsure(func() {
			rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
			rw.tc.Loaded.Config.Service = append(rw.tc.Loaded.Config.Service, config.Service{Name: "docs", Subdomain: "app"})
		})
		_, err := rw.up(t, commands.UpOptions{})
		requireRefusal(t, err, `Service docs answers where the host route web does`)
	})
}

func TestSharedServiceNamesAndHostRoutes(t *testing.T) {
	t.Run("up refuses a shared service named like an existing host route, before registering or starting anything", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		record, err := tc.Registry.Current(context.Background())
		if err != nil || record == nil {
			t.Fatal(err)
		}
		// A host route named like the shared service, which route add would
		// have refused: a record from before the shared service was declared.
		if _, err := registry.NewStore(w.ControlHome, record.RepositoryID).UpdateHostRoutes(context.Background(), record.WorktreeID, record.Slug,
			registry.Writer{Logger: tc.Logger, Invocation: "test", UserHome: w.UserHome},
			func(registry.RegisteredWorktree) ([]registry.HostRoute, error) {
				return []registry.HostRoute{{Name: "mailpit", Port: 9999, Subdomain: "mail2"}}, nil
			}); err != nil {
			t.Fatal(err)
		}
		before := len(w.Docker.Calls())
		ensures := w.router.Ensures()

		_, err = w.up(t, tc)
		requireRefusal(t, err, `Shared service mailpit has the name of a host route of this worktree, so the alias containers reach it by would mean two things\. Remove the route with 'treerig route rm mailpit', or rename the shared service\.`)
		if calls := w.Docker.Calls()[before:]; len(calls) != 0 {
			t.Fatalf("Docker was called after the refusal: %v", calls)
		}
		if w.router.Ensures() != ensures {
			t.Fatal("the router was asked to start")
		}
	})

	t.Run("shared up refuses it too", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		record, _ := tc.Registry.Current(context.Background())
		if _, err := registry.NewStore(w.ControlHome, record.RepositoryID).UpdateHostRoutes(context.Background(), record.WorktreeID, record.Slug,
			registry.Writer{Logger: tc.Logger, Invocation: "test", UserHome: w.UserHome},
			func(registry.RegisteredWorktree) ([]registry.HostRoute, error) {
				return []registry.HostRoute{{Name: "mailpit", Port: 9999, Subdomain: "mail2"}}, nil
			}); err != nil {
			t.Fatal(err)
		}
		err := commands.SharedUpWithContext(context.Background(), tc.CommandContext, w.router, w.index, commands.SharedUpOptions{})
		requireRefusal(t, err, `Shared service mailpit has the name of a host route`)
	})

	t.Run("route add refuses a declared shared name, the other direction", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		err := commands.RouteAddWithContext(context.Background(), tc.CommandContext, nil, commands.RouteAddOptions{Name: "mailpit", Port: 9999, Subdomain: "mail2"})
		requireRefusal(t, err, `mailpit is a shared service in treerig\.toml`)
	})
}

// TestHostRouteFilesAreNamedByIdentity pins the file name and the Traefik
// names of a route written by `route add`.
func TestHostRouteFilesAreNamedByIdentity(t *testing.T) {
	rw := hostRouteWorld(t)
	want := filepath.Join(rw.RouterState.DynamicDir, "host."+rw.ref.RepositoryID+"."+rw.ref.WorktreeID+".web.yaml")
	if rw.file("web") != want || !hasFile(want) {
		t.Fatalf("file = %q (exists %v), want %q", rw.file("web"), hasFile(rw.file("web")), want)
	}
	if got := router.HostRouteName(rw.ref.RepositoryID, rw.ref.WorktreeID, "web"); got != layout.ComposeProjectName(rw.ref.RepositoryID, rw.ref.WorktreeID)+"_host_web" {
		t.Fatalf("router name = %q", got)
	}
	_ = brand.Name
}
