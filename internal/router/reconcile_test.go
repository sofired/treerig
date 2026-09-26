package router_test

// Go-only, per #55: no TypeScript `it` to mirror — route fragments and
// their reconciliation are new in the Go build.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
)

func TestOrphanedFragments(t *testing.T) {
	t.Run("keeps a fragment whose worktree's router network is labelled, and returns one whose network is gone", func(t *testing.T) {
		live := router.FragmentInfo{RepositoryID: fragRepoA, WorktreeID: fragWtA, Service: "api"}
		gone := router.FragmentInfo{RepositoryID: fragRepoB, WorktreeID: fragWtB, Service: "web"}
		got := router.OrphanedFragments([]router.FragmentInfo{live, gone},
			[]string{layout.WorktreeRouterNetwork(fragRepoA, fragWtA), "some-other-network"})
		if len(got) != 1 || got[0].RepositoryID != fragRepoB || got[0].WorktreeID != fragWtB {
			t.Fatalf("orphaned = %+v, want only B's fragment", got)
		}
	})

	t.Run("returns nothing when there are no fragments", func(t *testing.T) {
		if got := router.OrphanedFragments(nil, []string{"x"}); len(got) != 0 {
			t.Fatalf("orphaned = %+v, want none", got)
		}
	})
}

func TestEnsureReconcilesRouteFragments(t *testing.T) {
	t.Run("removes the fragments of a worktree whose router network is gone, and keeps a live worktree's", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		liveNetwork := layout.WorktreeRouterNetwork(fragRepoA, fragWtA)
		w.Fake.networkLs = liveNetwork + "\n"
		// The router is attached to the live network, so Verify accepts it as is.
		doc := routerDoc(w.State.DynamicDir, w.Port)
		doc.NetworkSettings.Networks[liveNetwork] = json.RawMessage("{}")
		w.Fake.setDoc("router-1", doc)

		for _, f := range []struct{ repo, wt, service string }{
			{fragRepoA, fragWtA, "api"}, {fragRepoB, fragWtB, "api"}, {fragRepoB, fragWtB, "web"},
		} {
			if err := router.WriteFragment(w.State, f.repo, f.wt, f.service,
				router.NewFragmentDocument("n", "Host(`h`)", "n", 80)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		left, err := router.ListFragments(w.State)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 || left[0].RepositoryID != fragRepoA || left[0].Service != "api" {
			t.Fatalf("fragments left = %+v, want only A's api", left)
		}
	})

	t.Run("keeps every fragment when Docker cannot list the router's networks, rather than guessing them orphaned", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		if err := router.WriteFragment(w.State, fragRepoA, fragWtA, "api",
			router.NewFragmentDocument("n", "Host(`h`)", "n", 80)); err != nil {
			t.Fatal(err)
		}
		w.Fake.networkLsFails = true
		_ = w.Router.Ensure(context.Background())
		left, err := router.ListFragments(w.State)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 {
			t.Fatalf("fragments left = %+v, want A's untouched", left)
		}
	})
}
