package commands

import (
	"testing"

	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
)

func TestControlHomeRegistrationsLookup(t *testing.T) {
	const (
		repository = "11111111-1111-4111-8111-111111111111"
		worktree   = "22222222-2222-4222-8222-222222222221"
		missing    = "22222222-2222-4222-8222-222222222229"
	)
	controlHome := registrytest.TempDir(t)
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repository, WorktreeID: worktree, Slug: "feature-a", Domain: "acme.localhost",
		Initialization: "complete", Ports: map[string]int{"api": 30010},
	}); err != nil {
		t.Fatal(err)
	}
	regs := controlHomeRegistrations{controlHome: controlHome}

	t.Run("reports Registered with the recorded ports for a worktree that has a record", func(t *testing.T) {
		got, err := regs.Lookup(repository, worktree)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != router.Registered || got.Ports["api"] != 30010 {
			t.Fatalf("Lookup = %+v, want Registered with api 30010", got)
		}
	})

	t.Run("reports Unregistered for a known repository with no record for the worktree", func(t *testing.T) {
		got, err := regs.Lookup(repository, missing)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != router.Unregistered {
			t.Fatalf("Lookup = %+v, want Unregistered", got)
		}
	})

	t.Run("reports UnknownRepository for a repository directory this control home does not have", func(t *testing.T) {
		got, err := regs.Lookup("33333333-3333-4333-8333-333333333333", worktree)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != router.UnknownRepository {
			t.Fatalf("Lookup = %+v, want UnknownRepository", got)
		}
	})
}
