package worktree_test

import (
	"testing"

	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/worktree"
)

// str returns a pointer to s, for populating *string config fields inline.
func str(s string) *string { return &s }

// mixedServices is a root service (no subdomain, no path), a
// subdomain-only service, and a subdomain-plus-path service — the same
// shapes commands.TestWorktreeURLs covers, plus a hyphenated name to
// exercise the BERTH_SERVICE_URL_<NAME> mapping.
func mixedServices() []config.Service {
	return []config.Service{
		{Name: "api"},
		{Name: "mail-relay", Subdomain: "mail"},
		{Name: "docs", Subdomain: "docs", Path: str("/latest")},
	}
}

func TestRoutes(t *testing.T) {
	t.Run("routes the empty subdomain at the host root, prefixes the others, and folds in a non-default port", func(t *testing.T) {
		got := worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 8443)
		want := []string{
			"http://feature-login.acme.localhost:8443",
			"http://mail.feature-login.acme.localhost:8443",
			"http://docs.feature-login.acme.localhost:8443/latest",
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i, w := range want {
			if got[i].URL != w {
				t.Fatalf("index %d: got %q, want %q", i, got[i].URL, w)
			}
		}
	})

	t.Run("omits the port suffix for the default HTTP port, 80", func(t *testing.T) {
		got := worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 80)
		if got[0].URL != "http://feature-login.acme.localhost" {
			t.Fatalf("got %q", got[0].URL)
		}
	})

	t.Run("no services means no routes", func(t *testing.T) {
		if got := worktree.Routes(nil, "feature-login", "acme.localhost", 80); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRootRoute(t *testing.T) {
	t.Run("finds the service claiming the host root, with neither a subdomain nor a path", func(t *testing.T) {
		routes := worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 80)
		root := worktree.RootRoute(routes)
		if root == nil || root.Service != "api" || root.URL != "http://feature-login.acme.localhost" {
			t.Fatalf("got %+v", root)
		}
	})

	t.Run("is nil when no service claims the host root", func(t *testing.T) {
		services := []config.Service{
			{Name: "mail", Subdomain: "mail"},
			{Name: "api", Path: str("/api")}, // shares the host root, but only under a path
		}
		routes := worktree.Routes(services, "feature-login", "acme.localhost", 80)
		if root := worktree.RootRoute(routes); root != nil {
			t.Fatalf("got %+v, want nil", root)
		}
	})

	t.Run("is nil for no routes at all", func(t *testing.T) {
		if root := worktree.RootRoute(nil); root != nil {
			t.Fatalf("got %+v, want nil", root)
		}
	})
}

func TestServiceURLVariables(t *testing.T) {
	t.Run("BERTH_URL for the root route plus BERTH_SERVICE_URL_<NAME> for every routed service, name upper-cased with hyphens turned to underscores", func(t *testing.T) {
		routes := worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 8443)
		got := pairsToMap(worktree.ServiceURLVariables(routes))
		want := map[string]string{
			"URL":                    "http://feature-login.acme.localhost:8443",
			"SERVICE_URL_API":        "http://feature-login.acme.localhost:8443",
			"SERVICE_URL_MAIL_RELAY": "http://mail.feature-login.acme.localhost:8443",
			"SERVICE_URL_DOCS":       "http://docs.feature-login.acme.localhost:8443/latest",
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("no URL variable at all when nothing claims the host root", func(t *testing.T) {
		services := []config.Service{{Name: "mail", Subdomain: "mail"}}
		routes := worktree.Routes(services, "feature-login", "acme.localhost", 80)
		got := pairsToMap(worktree.ServiceURLVariables(routes))
		if _, ok := got["URL"]; ok {
			t.Fatalf("got a URL variable: %+v", got)
		}
		if got["SERVICE_URL_MAIL"] != "http://mail.feature-login.acme.localhost" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("no variables at all for no routes", func(t *testing.T) {
		if got := worktree.ServiceURLVariables(nil); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("BERTH_SERVICE_PORT_<NAME> for every service with an allocated port, named as BERTH_SERVICE_URL_<NAME> is (#119)", func(t *testing.T) {
		routes := worktree.WithPorts(worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 8443),
			map[string]int{"api": 20001, "mail-relay": 20002})
		got := pairsToMap(worktree.ServiceURLVariables(routes))
		if got["SERVICE_PORT_API"] != "20001" || got["SERVICE_PORT_MAIL_RELAY"] != "20002" {
			t.Fatalf("got %+v", got)
		}
		if _, ok := got["SERVICE_PORT_DOCS"]; ok {
			t.Fatalf("docs has no port but got a variable: %+v", got)
		}
		// The URLs are still the hostname URLs through the router.
		if got["SERVICE_URL_API"] != "http://feature-login.acme.localhost:8443" || got["URL"] != "http://feature-login.acme.localhost:8443" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("WithPorts leaves the routes it was given untouched", func(t *testing.T) {
		routes := worktree.Routes(mixedServices(), "feature-login", "acme.localhost", 80)
		_ = worktree.WithPorts(routes, map[string]int{"api": 20001})
		for _, route := range routes {
			if route.Port != 0 {
				t.Fatalf("%s: Port = %d", route.Service, route.Port)
			}
		}
	})
}

// pairsToMap turns worktree.ServiceURLVariables' [][2]string pairs into a
// map, for asserting on individual variables without caring about order.
func pairsToMap(pairs [][2]string) map[string]string {
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		m[pair[0]] = pair[1]
	}
	return m
}

func TestSharedAddrVariables(t *testing.T) {
	addrs := []worktree.SharedAddr{
		{Name: "mailpit", Port: 1025, Addr: "127.0.0.1:30001"},
		{Name: "mailpit", Port: 8025, Addr: "127.0.0.1:30002"},
		{Name: "object-store", Port: 9000, Addr: "127.0.0.1:30003"},
	}
	t.Run("one variable per pinned address, name upper-cased with hyphens turned into underscores", func(t *testing.T) {
		got := pairsToMap(worktree.SharedAddrVariables(addrs))
		want := map[string]string{
			"SHARED_ADDR_MAILPIT_1025":      "127.0.0.1:30001",
			"SHARED_ADDR_MAILPIT_8025":      "127.0.0.1:30002",
			"SHARED_ADDR_OBJECT_STORE_9000": "127.0.0.1:30003",
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("no two names and ports share a variable, though a name may end in digits", func(t *testing.T) {
		got := worktree.SharedAddrVariables([]worktree.SharedAddr{
			{Name: "mail", Port: 25, Addr: "a"}, {Name: "mail-25", Port: 25, Addr: "b"}, {Name: "mail", Port: 2525, Addr: "c"},
		})
		seen := map[string]bool{}
		for _, pair := range got {
			if seen[pair[0]] {
				t.Fatalf("%s twice in %v", pair[0], got)
			}
			seen[pair[0]] = true
		}
	})

	t.Run("none for no addresses, and Variables adds the BERTH_ prefix", func(t *testing.T) {
		if got := worktree.SharedAddrVariables(nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
		if got := worktree.Variables(worktree.SharedAddrVariables(addrs[:1])); got[0] != "BERTH_SHARED_ADDR_MAILPIT_1025=127.0.0.1:30001" {
			t.Fatalf("got %v", got)
		}
		if !worktree.IsSharedAddrVariable("BERTH_SHARED_ADDR_X_1") || worktree.IsSharedAddrVariable("BERTH_SERVICE_URL_X") {
			t.Fatal("IsSharedAddrVariable is wrong")
		}
	})
}
