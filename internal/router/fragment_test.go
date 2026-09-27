package router_test

// Dynamic-configuration fragments, added by #55.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

// fragmentState is a layout.RouterLayout rooted at a fresh temp directory,
// with only DynamicDir set — the one field every fragment function needs.
func fragmentState(t *testing.T) layout.RouterLayout {
	t.Helper()
	dir := registrytest.TempDir(t)
	return layout.RouterLayout{DynamicDir: filepath.Join(dir, "dynamic")}
}

const (
	fragRepoA = "11111111-1111-1111-1111-111111111111"
	fragWtA   = "22222222-2222-2222-2222-222222222222"
	fragRepoB = "33333333-3333-3333-3333-333333333333"
	fragWtB   = "44444444-4444-4444-4444-444444444444"
)

func TestFragmentPath(t *testing.T) {
	t.Run("names one file per repository, worktree and service, splitting back into its three parts", func(t *testing.T) {
		state := fragmentState(t)
		path := router.FragmentPath(state, fragRepoA, fragWtA, "api")
		want := filepath.Join(state.DynamicDir, fragRepoA+"."+fragWtA+".api.yaml")
		if path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})

	t.Run("keeps a service name that itself contains a dot as one field", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("x", "Host(`x`)", "x", 8080)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "a.b", doc); err != nil {
			t.Fatal(err)
		}
		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 || fragments[0].Service != "a.b" {
			t.Fatalf("fragments = %+v, want one with service \"a.b\"", fragments)
		}
	})
}

func TestWriteFragment(t *testing.T) {
	t.Run("writes a Traefik dynamic-configuration document at 0644, JSON that Traefik's file provider reads as YAML", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("acme-api", "Host(`acme.localhost`)", "acme-api", 8080)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", doc); err != nil {
			t.Fatal(err)
		}
		path := router.FragmentPath(state, fragRepoA, fragWtA, "api")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %o, want 0644", info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, data)
		}
		http, ok := decoded["http"].(map[string]any)
		if !ok {
			t.Fatalf("no http section: %v", decoded)
		}
		routers, ok := http["routers"].(map[string]any)
		if !ok {
			t.Fatalf("no http.routers: %v", http)
		}
		acmeAPI, ok := routers["acme-api"].(map[string]any)
		if !ok {
			t.Fatalf("no router acme-api: %v", routers)
		}
		if acmeAPI["rule"] != "Host(`acme.localhost`)" {
			t.Fatalf("rule = %v", acmeAPI["rule"])
		}
		services, ok := http["services"].(map[string]any)
		if !ok {
			t.Fatalf("no http.services: %v", http)
		}
		svc, ok := services["acme-api"].(map[string]any)
		if !ok {
			t.Fatalf("no service acme-api: %v", services)
		}
		lb, ok := svc["loadBalancer"].(map[string]any)
		if !ok {
			t.Fatalf("no loadBalancer: %v", svc)
		}
		servers, ok := lb["servers"].([]any)
		if !ok || len(servers) != 1 {
			t.Fatalf("servers = %v", lb["servers"])
		}
		server, ok := servers[0].(map[string]any)
		if !ok || server["url"] != "http://acme-api:8080" {
			t.Fatalf("server = %v, want url http://acme-api:8080", server)
		}
	})

	t.Run("overwrites in place on a repeated write, rather than leaving a stale copy", func(t *testing.T) {
		state := fragmentState(t)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", router.NewFragmentDocument("x", "Host(`old`)", "x", 80)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", router.NewFragmentDocument("x", "Host(`new`)", "x", 80)); err != nil {
			t.Fatal(err)
		}
		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 {
			t.Fatalf("fragments = %+v, want exactly one", fragments)
		}
	})
}

func TestRemoveWorktreeFragments(t *testing.T) {
	t.Run("removes every fragment for one worktree, leaving another worktree's and the index's own alone", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("x", "Host(`x`)", "x", 80)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "mail", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoB, fragWtB, "web", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteIndexFragment(state, router.DefaultName); err != nil {
			t.Fatal(err)
		}

		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatal(err)
		}

		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 || fragments[0].RepositoryID != fragRepoB {
			t.Fatalf("fragments = %+v, want only the other worktree's", fragments)
		}
		if exists, err := os.Stat(router.IndexFragmentPath(state)); err != nil || exists.IsDir() {
			t.Fatalf("expected the index's own fragment to survive untouched: %v", err)
		}
	})

	t.Run("tolerates a directory that does not exist yet, and a worktree with nothing to remove", func(t *testing.T) {
		state := fragmentState(t)
		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatalf("removing from a directory that was never created: %v", err)
		}
		if err := router.WriteFragment(state, fragRepoB, fragWtB, "api", router.NewFragmentDocument("x", "Host(`x`)", "x", 80)); err != nil {
			t.Fatal(err)
		}
		if err := router.RemoveWorktreeFragments(state, fragRepoA, fragWtA); err != nil {
			t.Fatalf("removing a worktree with nothing staged: %v", err)
		}
		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 {
			t.Fatalf("expected the other worktree's fragment to survive, got %+v", fragments)
		}
	})
}

// Round 2 of #55's review: `up` used to remove every fragment it owned
// before writing the new ones, which briefly unrouted every one of a
// worktree's own routes on every re-up, and only ran that removal at all
// when the config currently declared at least one [[service]] — so a
// worktree whose last route was dropped kept its stale fragment forever.
// RemoveWorktreeFragmentsExcept is the fix `up` now calls after writing
// every current route's fragment, naming those as keep.
func TestRemoveWorktreeFragmentsExcept(t *testing.T) {
	t.Run("removes only this worktree's own fragments whose service is not in keep, leaving another worktree's alone", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("x", "Host(`x`)", "x", 80)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "mail", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoB, fragWtB, "web", doc); err != nil {
			t.Fatal(err)
		}

		if err := router.RemoveWorktreeFragmentsExcept(state, fragRepoA, fragWtA, map[string]bool{"api": true}); err != nil {
			t.Fatal(err)
		}

		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		var services []string
		for _, fragment := range fragments {
			services = append(services, fragment.RepositoryID+"/"+fragment.Service)
		}
		want := []string{fragRepoA + "/api", fragRepoB + "/web"}
		if len(services) != len(want) {
			t.Fatalf("fragments = %v, want %v", services, want)
		}
		for _, w := range want {
			if !slices.Contains(services, w) {
				t.Fatalf("fragments = %v, want %v", services, want)
			}
		}
	})

	t.Run("removes every fragment this worktree owns when keep is nil or empty, the same as RemoveWorktreeFragments", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("x", "Host(`x`)", "x", 80)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", doc); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, fragRepoB, fragWtB, "web", doc); err != nil {
			t.Fatal(err)
		}

		if err := router.RemoveWorktreeFragmentsExcept(state, fragRepoA, fragWtA, nil); err != nil {
			t.Fatal(err)
		}

		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 || fragments[0].RepositoryID != fragRepoB {
			t.Fatalf("fragments = %+v, want only the other worktree's", fragments)
		}
	})

	t.Run("recovers a service name carrying a dot exactly, rather than truncating it", func(t *testing.T) {
		state := fragmentState(t)
		doc := router.NewFragmentDocument("x", "Host(`x`)", "x", 80)
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api.internal", doc); err != nil {
			t.Fatal(err)
		}

		if err := router.RemoveWorktreeFragmentsExcept(state, fragRepoA, fragWtA, map[string]bool{"api.internal": true}); err != nil {
			t.Fatal(err)
		}

		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 || fragments[0].Service != "api.internal" {
			t.Fatalf("fragments = %+v, want the dotted service name kept whole", fragments)
		}
	})
}

func TestListFragments(t *testing.T) {
	t.Run("tolerates an empty or missing directory", func(t *testing.T) {
		state := fragmentState(t)
		fragments, err := router.ListFragments(state)
		if err != nil || fragments != nil {
			t.Fatalf("fragments = %+v, err = %v, want (nil, nil)", fragments, err)
		}
	})

	t.Run("skips a filename that does not parse as <uuid>.<uuid>.<service>.yaml, including the index's own", func(t *testing.T) {
		state := fragmentState(t)
		if err := router.WriteIndexFragment(state, router.DefaultName); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(state.DynamicDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"not-a-uuid.also-not.api.yaml", "README.md", fragRepoA + "." + fragWtA} {
			if err := os.WriteFile(filepath.Join(state.DynamicDir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := router.WriteFragment(state, fragRepoA, fragWtA, "api", router.NewFragmentDocument("x", "Host(`x`)", "x", 80)); err != nil {
			t.Fatal(err)
		}

		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 || fragments[0].RepositoryID != fragRepoA || fragments[0].WorktreeID != fragWtA || fragments[0].Service != "api" {
			t.Fatalf("fragments = %+v, want exactly the one well-formed entry", fragments)
		}
	})
}

func TestIndexFragmentDocument(t *testing.T) {
	t.Run("routes the checkout index's own host to its container by name, never to a service name a worktree could also use", func(t *testing.T) {
		doc := router.IndexFragmentDocument("berth-router")
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		http := decoded["http"].(map[string]any)
		services := http["services"].(map[string]any)
		berthIndex, ok := services["berth-index"].(map[string]any)
		if !ok {
			t.Fatalf("no berth-index service: %v", services)
		}
		lb := berthIndex["loadBalancer"].(map[string]any)
		servers := lb["servers"].([]any)
		server := servers[0].(map[string]any)
		want := "http://berth-router-index-1:" + strconv.Itoa(router.IndexPort)
		if server["url"] != want {
			t.Fatalf("url = %v, want %v", server["url"], want)
		}
	})
}
