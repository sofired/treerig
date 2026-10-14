package commands

import (
	"context"
	"testing"
	"time"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/worktree"
)

func TestTakeBackHostRouteSurvivesACancelledCaller(t *testing.T) {
	controlHome := registrytest.TempDir(t) + "/control"
	registrytest.ClaimControlHome(t, controlHome)
	const repo, wt = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222221"
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repo, WorktreeID: wt, Slug: "feature-a", Domain: "acme.localhost", Initialization: "complete",
		HostRoutes: []registrytest.HostRouteJSON{{Name: "web", Port: 5173}, {Name: "keep", Port: 5174, Subdomain: "k"}},
	}); err != nil {
		t.Fatal(err)
	}
	cctx := &CommandContext{Logger: &logger.Recording{}, Invocation: "berth route add", UserHome: registrytest.TempDir(t)}
	store := registry.NewStore(controlHome, repo)

	// The caller's context is already cancelled, as it is when a command was
	// interrupted while it wrote the file: the undo must still run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	takeBackHostRoute(ctx, cctx, store, worktree.Ref{RepositoryID: repo, WorktreeID: wt, Slug: "feature-a"}, "web")
	if time.Since(start) > 4*time.Second {
		t.Fatal("the undo waited out its bound")
	}

	record, err := store.Find(wt)
	if err != nil || record == nil {
		t.Fatalf("Find = %+v, %v", record, err)
	}
	if len(record.HostRoutes) != 1 || record.HostRoutes[0].Name != "keep" {
		t.Fatalf("host routes = %+v, want only keep", record.HostRoutes)
	}
}
