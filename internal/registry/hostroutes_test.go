package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// strPtr is &s, for a route's optional path.
func strPtr(s string) *string { return &s }

// hostRouteFixture is a record of wtA carrying routes and ports.
func hostRouteFixture(t *testing.T, controlHome string, routes []registrytest.HostRouteJSON, ports map[string]int) *registry.Store {
	t.Helper()
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repoA, WorktreeID: wtA, Slug: "feature-login", Domain: acmeDomain,
		Initialization: "complete", Ports: ports, HostRoutes: routes,
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	return registry.NewStore(controlHome, repoA)
}

func TestUpdateHostRoutes(t *testing.T) {
	ctx := context.Background()
	add := func(route registry.HostRoute) func(registry.RegisteredWorktree) ([]registry.HostRoute, error) {
		return func(record registry.RegisteredWorktree) ([]registry.HostRoute, error) {
			return append(append([]registry.HostRoute{}, record.HostRoutes...), route), nil
		}
	}

	t.Run("stores the routes sorted by name and reads them back", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := hostRouteFixture(t, controlHome, nil, nil)
		for _, route := range []registry.HostRoute{
			{Name: "web", Port: 5173},
			{Name: "api", Port: 8080, Subdomain: "api", Path: strPtr("/v1")},
		} {
			if _, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter, add(route)); err != nil {
				t.Fatal(err)
			}
		}
		record, err := store.Find(wtA)
		if err != nil || record == nil {
			t.Fatalf("Find = %+v, %v", record, err)
		}
		want := []registry.HostRoute{
			{Name: "api", Port: 8080, Subdomain: "api", Path: strPtr("/v1")},
			{Name: "web", Port: 5173},
		}
		if !reflect.DeepEqual(record.HostRoutes, want) {
			t.Fatalf("HostRoutes = %+v, want %+v", record.HostRoutes, want)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("removing the last route leaves a record without the key", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 5173}}, nil)
		routes, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter,
			func(registry.RegisteredWorktree) ([]registry.HostRoute, error) { return nil, nil })
		if err != nil || routes != nil {
			t.Fatalf("UpdateHostRoutes = %v, %v", routes, err)
		}
		data, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "hostRoutes") {
			t.Fatalf("a record with no host routes should not name the key:\n%s", data)
		}
	})

	t.Run("writes nothing when the list is unchanged", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 5173}}, nil)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(store.RecordPath(wtA), old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter,
			func(record registry.RegisteredWorktree) ([]registry.HostRoute, error) { return record.HostRoutes, nil }); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(store.RecordPath(wtA))
		if err != nil || info.ModTime().After(old.Add(time.Minute)) {
			t.Fatalf("the record was rewritten: %v, %v", info, err)
		}
	})

	t.Run("a refusal from change writes nothing and passes through", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 5173}}, nil)
		before, _ := os.ReadFile(store.RecordPath(wtA))
		refusal := errors.New("not today")
		_, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter,
			func(registry.RegisteredWorktree) ([]registry.HostRoute, error) { return nil, refusal })
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v", err)
		}
		after, _ := os.ReadFile(store.RecordPath(wtA))
		if string(before) != string(after) {
			t.Fatalf("the record changed:\n%s\n%s", before, after)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("hands change the record as it stands under the lock", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 5173}}, map[string]int{"api": 30010})
		var seen registry.RegisteredWorktree
		_, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter, func(record registry.RegisteredWorktree) ([]registry.HostRoute, error) {
			seen = record
			return record.HostRoutes, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen.Ports["api"] != 30010 || len(seen.HostRoutes) != 1 || seen.HostRoutes[0].Name != "web" {
			t.Fatalf("change saw %+v", seen)
		}
	})

	t.Run("refuses a record that no longer carries the expected slug", func(t *testing.T) {
		store := hostRouteFixture(t, claimedHome(t), nil, nil)
		_, err := store.UpdateHostRoutes(ctx, wtA, "someone-else", testWriter, add(registry.HostRoute{Name: "web", Port: 1}))
		if err == nil || !strings.Contains(err.Error(), "no longer owns") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses a record that does not exist", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		_, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter, add(registry.HostRoute{Name: "web", Port: 1}))
		if err == nil || !strings.Contains(err.Error(), "registration is missing") {
			t.Fatalf("err = %v", err)
		}
	})

	for name, route := range map[string]registry.HostRoute{
		"an uppercase name":         {Name: "Web", Port: 5173},
		"port zero":                 {Name: "web", Port: 0},
		"a port above 65535":        {Name: "web", Port: 65536},
		"a subdomain with a dot":    {Name: "web", Port: 5173, Subdomain: "a.b"},
		"a path without a slash":    {Name: "web", Port: 5173, Path: strPtr("v1")},
		"a path with a backtick":    {Name: "web", Port: 5173, Path: strPtr("/a`b")},
		"a name over 63 characters": {Name: strings.Repeat("a", 64), Port: 5173},
	} {
		t.Run("refuses to record "+name, func(t *testing.T) {
			store := hostRouteFixture(t, claimedHome(t), nil, nil)
			if _, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter, add(route)); err == nil {
				t.Fatal("recorded an invalid route")
			}
			record, _ := store.Find(wtA)
			if len(record.HostRoutes) != 0 {
				t.Fatalf("HostRoutes = %+v", record.HostRoutes)
			}
		})
	}

	t.Run("refuses the same name twice", func(t *testing.T) {
		store := hostRouteFixture(t, claimedHome(t), nil, nil)
		_, err := store.UpdateHostRoutes(ctx, wtA, "feature-login", testWriter, func(registry.RegisteredWorktree) ([]registry.HostRoute, error) {
			return []registry.HostRoute{{Name: "web", Port: 1}, {Name: "web", Port: 2}}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "listed twice") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRecordHostRoutesSurviveEveryWriter(t *testing.T) {
	ctx := context.Background()
	routes := []registrytest.HostRouteJSON{{Name: "api", Port: 8080, Subdomain: "api", Path: strPtr("/v1")}, {Name: "web", Port: 5173}}
	want := []registry.HostRoute{{Name: "api", Port: 8080, Subdomain: "api", Path: strPtr("/v1")}, {Name: "web", Port: 5173}}
	allocate := func(_ []registry.RegisteredWorktree, preferred string) (string, error) {
		if preferred != "" {
			return preferred, nil
		}
		return "feature-login", nil
	}
	initialize := func() (registry.WorktreeInitialization, error) { return registry.Complete, nil }
	namespace := "dev.other"

	writers := map[string]func(t *testing.T, store *registry.Store){
		"Ensure's re-domain": func(t *testing.T, store *registry.Store) {
			if _, err := store.Ensure(ctx, wtA, "new.localhost", allocate, testWriter, initialize, nil); err != nil {
				t.Fatal(err)
			}
		},
		"Ensure renaming the slug": func(t *testing.T, store *registry.Store) {
			rename := func([]registry.RegisteredWorktree, string) (string, error) { return "renamed", nil }
			if _, err := store.Ensure(ctx, wtA, "new.localhost", rename, testWriter, initialize, nil); err != nil {
				t.Fatal(err)
			}
		},
		"Ensure's label rewrite": func(t *testing.T, store *registry.Store) {
			if _, err := store.Ensure(ctx, wtA, acmeDomain, allocate, testWriter, initialize, &namespace); err != nil {
				t.Fatal(err)
			}
		},
		"MarkPending": func(t *testing.T, store *registry.Store) {
			if err := store.MarkPending(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		},
		"SyncPorts allocating": func(t *testing.T, store *registry.Store) {
			if _, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, registry.PortPlan{
				Services: []string{"worker"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
			}); err != nil {
				t.Fatal(err)
			}
		},
		"ReleasePorts": func(t *testing.T, store *registry.Store) {
			if err := store.ReleasePorts(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			controlHome := claimedHome(t)
			store := hostRouteFixture(t, controlHome, routes, map[string]int{"api": 30010})
			write(t, store)
			record, err := store.Find(wtA)
			if err != nil || record == nil {
				t.Fatalf("Find = %+v, %v", record, err)
			}
			if !reflect.DeepEqual(record.HostRoutes, want) {
				t.Fatalf("host routes after %s = %+v, want %+v", name, record.HostRoutes, want)
			}
		})
	}
}

func TestRecordHostRoutesShape(t *testing.T) {
	record := func(routes string) string {
		return `{"version":1,"repositoryId":"` + repoA + `","worktreeId":"` + wtA + `","slug":"x","domain":"d","initialization":"complete"` + routes + `}`
	}
	read := func(t *testing.T, body string) (*registry.RegisteredWorktree, error) {
		t.Helper()
		controlHome := registrytest.TempDir(t)
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return registry.NewStore(controlHome, repoA).Find(wtA)
	}

	t.Run("a record without host routes stays valid", func(t *testing.T) {
		found, err := read(t, record(""))
		if err != nil || found == nil || found.HostRoutes != nil {
			t.Fatalf("Find = %+v, %v", found, err)
		}
	})
	t.Run("an empty list reads as none", func(t *testing.T) {
		found, err := read(t, record(`,"hostRoutes":[]`))
		if err != nil || found == nil || found.HostRoutes != nil {
			t.Fatalf("Find = %+v, %v", found, err)
		}
	})
	t.Run("a valid route is read in every field", func(t *testing.T) {
		found, err := read(t, record(`,"hostRoutes":[{"name":"web","port":5173,"subdomain":"app","path":"/x"}]`))
		want := []registry.HostRoute{{Name: "web", Port: 5173, Subdomain: "app", Path: strPtr("/x")}}
		if err != nil || found == nil || !reflect.DeepEqual(found.HostRoutes, want) {
			t.Fatalf("Find = %+v, %v", found, err)
		}
	})
	for name, routes := range map[string]string{
		"a missing name":        `,"hostRoutes":[{"port":5173}]`,
		"a missing port":        `,"hostRoutes":[{"name":"web"}]`,
		"a string port":         `,"hostRoutes":[{"name":"web","port":"5173"}]`,
		"a fractional port":     `,"hostRoutes":[{"name":"web","port":5173.5}]`,
		"port zero":             `,"hostRoutes":[{"name":"web","port":0}]`,
		"an unknown key":        `,"hostRoutes":[{"name":"web","port":5173,"host":"x"}]`,
		"a bad name":            `,"hostRoutes":[{"name":"Web!","port":5173}]`,
		"a bad subdomain":       `,"hostRoutes":[{"name":"web","port":5173,"subdomain":"-a"}]`,
		"a bad path":            `,"hostRoutes":[{"name":"web","port":5173,"path":"x"}]`,
		"a name twice":          `,"hostRoutes":[{"name":"web","port":1},{"name":"web","port":2}]`,
		"an object, not a list": `,"hostRoutes":{"web":5173}`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := read(t, record(routes))
			if err == nil || !strings.Contains(err.Error(), "unrecognized version or shape") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestHostRoutesAreNotHandedOutAsPorts(t *testing.T) {
	ctx := context.Background()
	controlHome := claimedHome(t)
	// A three-port range with two ports named by host routes leaves one.
	store := hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 30100}, {Name: "api", Port: 30101}}, nil)
	result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, registry.PortPlan{
		Services: []string{"worker"}, Range: smallRange, Probe: registrytest.FreeProbe{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ports["worker"] != 30102 {
		t.Fatalf("worker got %d, want the one port no host route names", result.Ports["worker"])
	}
}

func TestPortHolder(t *testing.T) {
	controlHome := claimedHome(t)
	hostRouteFixture(t, controlHome, []registrytest.HostRouteJSON{{Name: "web", Port: 5173}}, map[string]int{"api": 30010, "web": 30011})

	t.Run("names the service holding a pinned port", func(t *testing.T) {
		holder, err := registry.PortHolder(controlHome, 30011)
		if err != nil || holder != "service web of feature-login."+acmeDomain {
			t.Fatalf("PortHolder = %q, %v", holder, err)
		}
	})
	t.Run("a port a host route names is not held", func(t *testing.T) {
		holder, err := registry.PortHolder(controlHome, 5173)
		if err != nil || holder != "" {
			t.Fatalf("PortHolder = %q, %v", holder, err)
		}
	})
	t.Run("names a shared service's pinned port", func(t *testing.T) {
		store := registry.NewStore(controlHome, repoA)
		result := syncShared(t, store, "mailpit", []int{1025}, nil)
		holder, err := registry.PortHolder(controlHome, result.Ports[1025])
		if err != nil || holder != "shared service mailpit" {
			t.Fatalf("PortHolder = %q, %v", holder, err)
		}
	})
	t.Run("refuses while a record cannot be read", func(t *testing.T) {
		broken := claimedHome(t)
		dir := registry.RegistryDirectory(broken, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.PortHolder(broken, 5173); err == nil || !strings.Contains(err.Error(), "unreadable records") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestHostRouteSame(t *testing.T) {
	base := registry.HostRoute{Name: "web", Port: 1, Subdomain: "app", Path: strPtr("/x")}
	if !base.Same(registry.HostRoute{Name: "web", Port: 1, Subdomain: "app", Path: strPtr("/x")}) {
		t.Fatal("identical routes differ")
	}
	for name, other := range map[string]registry.HostRoute{
		"name": {Name: "w", Port: 1, Subdomain: "app", Path: strPtr("/x")}, "port": {Name: "web", Port: 2, Subdomain: "app", Path: strPtr("/x")},
		"subdomain": {Name: "web", Port: 1, Subdomain: "b", Path: strPtr("/x")}, "path": {Name: "web", Port: 1, Subdomain: "app", Path: strPtr("/y")},
		"no path": {Name: "web", Port: 1, Subdomain: "app"},
	} {
		if base.Same(other) {
			t.Errorf("a different %s counts as the same", name)
		}
	}
}

func TestHostRouteSamePlace(t *testing.T) {
	cases := []struct {
		name string
		a, b registry.HostRoute
		same bool
	}{
		{"the host root twice", registry.HostRoute{}, registry.HostRoute{Port: 9}, true},
		{"the same subdomain", registry.HostRoute{Subdomain: "api"}, registry.HostRoute{Subdomain: "api"}, true},
		{"different subdomains", registry.HostRoute{Subdomain: "api"}, registry.HostRoute{Subdomain: "web"}, false},
		{"the same path", registry.HostRoute{Path: strPtr("/a")}, registry.HostRoute{Path: strPtr("/a")}, true},
		{"different paths", registry.HostRoute{Path: strPtr("/a")}, registry.HostRoute{Path: strPtr("/b")}, false},
		{"a path and none", registry.HostRoute{Path: strPtr("/a")}, registry.HostRoute{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.SamePlace(c.b); got != c.same {
				t.Fatalf("SamePlace = %v, want %v", got, c.same)
			}
		})
	}
}
