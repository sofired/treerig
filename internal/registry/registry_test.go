package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
)

// registryFixture is a hermetic world for Registry tests: a fake Git runner
// and a real filesystem control home, but never the (locked) write path —
// registrations are built with registrytest.Register, a raw fixture write
// straight to disk.
type registryFixture struct {
	base, controlHome, root, common string
	git                             *registrytest.GitRunner
	runner                          run.Runner
	registry                        *registry.Registry
	log                             *logger.Recording
}

// newRegistryFixture builds a worktree and its git common directory under a
// fresh temp control home, registers them with a fake git runner on branch,
// and returns a registryFixture with a Registry opened for domain.
func newRegistryFixture(t *testing.T, branch, domain string) registryFixture {
	t.Helper()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	root := filepath.Join(base, "worktree")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(common, "worktrees", "worktree"), 0o700); err != nil {
		t.Fatalf("mkdir git dir: %v", err)
	}
	runner, g := registrytest.NewFakeRunner()
	g.Add(root, branch, common)
	log := &logger.Recording{}
	reg := registry.New(registry.Options{
		ControlHome: controlHome, Root: root, Domain: domain, Runner: runner, Logger: log,
		UserHome: filepath.Join(base, "user-home"),
	})
	return registryFixture{base: base, controlHome: controlHome, root: root, common: common, git: g, runner: runner, registry: reg, log: log}
}

func TestRegistryCurrent(t *testing.T) {
	ctx := context.Background()

	t.Run("answers nil when this checkout is not registered", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		current, err := f.registry.Current(ctx)
		if err != nil || current != nil {
			t.Fatalf("expected (nil, nil), got (%+v, %v)", current, err)
		}
	})

	t.Run("reads an existing registration", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		identity, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		current, err := f.registry.Current(ctx)
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		if current == nil || current.Slug != "feature-login" || current.Domain != acmeDomain ||
			current.RepositoryID != identity.RepositoryID || current.WorktreeID != identity.WorktreeID ||
			current.Root != f.root || current.Branch != "feature/login" {
			t.Fatalf("unexpected registration: %+v", current)
		}
		if _, printed := f.log.Snapshot(); len(printed) != 0 {
			t.Fatalf("expected nothing on stdout")
		}
	})

	t.Run("warns, but does not repair, when the record disagrees with the configured domain", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", "new.localhost")
		if _, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete"); err != nil {
			t.Fatalf("Register: %v", err)
		}

		current, err := f.registry.Current(ctx)
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		if current == nil || current.Domain != acmeDomain {
			t.Fatalf("Current must report the record as it stands, not the configured domain: %+v", current)
		}
		lines, _ := f.log.Snapshot()
		var warned bool
		for _, line := range lines {
			if contains(line, "warn:") && contains(line, "now says new.localhost") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("expected a warning about the domain mismatch, got %+v", lines)
		}
	})
}

func TestRegistryListAndListAll(t *testing.T) {
	ctx := context.Background()

	t.Run("list is empty before this repository has a repository identity", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		list, err := f.registry.List(ctx)
		if err != nil || list != nil {
			t.Fatalf("expected (nil, nil), got (%+v, %v)", list, err)
		}
	})

	t.Run("list is this repository's own registrations only", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		identity, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		// A record from a different repository, sharing the control home.
		if err := writeOtherRepositoryRecord(f.controlHome); err != nil {
			t.Fatalf("fixture record: %v", err)
		}

		list, err := f.registry.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 || list[0].WorktreeID != identity.WorktreeID {
			t.Fatalf("expected only this repository's record, got %+v", list)
		}
	})

	t.Run("listAll spans repositories and warns about unreadable records instead of failing", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		if _, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := writeOtherRepositoryRecord(f.controlHome); err != nil {
			t.Fatalf("fixture record: %v", err)
		}
		badDir := registry.RegistryDirectory(f.controlHome, repoB)
		if err := os.MkdirAll(badDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(badDir, "c2d3e4f5-2222-4333-8444-555566667778.json"), []byte("{ not json"), 0o600); err != nil {
			t.Fatalf("write bad record: %v", err)
		}

		all, err := f.registry.ListAll(ctx)
		if err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("expected 2 readable records, got %+v", all)
		}
		lines, _ := f.log.Snapshot()
		var warned bool
		for _, line := range lines {
			if contains(line, "warn:") && contains(line, "Skipping unreadable registry record") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("expected a warning about the unreadable record, got %+v", lines)
		}
	})
}

// writeOtherRepositoryRecord plants a fixture record for a second
// repository (repoB/wtB) under controlHome, for tests asserting a lookup
// finds it across repositories.
func writeOtherRepositoryRecord(controlHome string) error {
	_, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repoB, WorktreeID: wtB, Slug: "other-clone", Domain: "widgets.localhost", Initialization: "complete",
	})
	return err
}

func TestRegistryPruneCandidates(t *testing.T) {
	ctx := context.Background()

	t.Run("no repository identity yet prunes nothing", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		candidates, err := f.registry.PruneCandidates(ctx)
		if err != nil || candidates != nil {
			t.Fatalf("expected (nil, nil), got (%+v, %v)", candidates, err)
		}
	})

	t.Run("excludes an active worktree and includes one Git no longer lists", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		activeIdentity, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		// A second worktree of the same repository, registered, then no
		// longer reported by `git worktree list` (as after `git worktree
		// remove`).
		gone := filepath.Join(f.base, "gone")
		if err := os.MkdirAll(filepath.Join(f.common, "worktrees", "gone"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.MkdirAll(gone, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f.git.Add(gone, "feature/gone", f.common)
		goneIdentities := registry.NewIdentityService(gone, f.runner, nil)
		goneIdentity, err := registrytest.Register(ctx, f.controlHome, goneIdentities, gone, acmeDomain, "feature-gone", nil, "complete")
		if err != nil {
			t.Fatalf("Register gone: %v", err)
		}
		f.git.SetActive([]string{f.root}) // simulate `git worktree remove gone`

		candidates, err := f.registry.PruneCandidates(ctx)
		if err != nil {
			t.Fatalf("PruneCandidates: %v", err)
		}
		if len(candidates) != 1 || candidates[0].WorktreeID != goneIdentity.WorktreeID {
			t.Fatalf("expected only the gone worktree, got %+v (active=%s)", candidates, activeIdentity.WorktreeID)
		}
	})
}
