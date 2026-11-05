package registry_test

// Tests Registry.Ensure and this package's reconcile behaviour.
// Two scenarios are left out as redundant: a symlinked identity-marker
// parent rejected before creating markers, and ignoring active sibling
// worktrees that do not have a worktree marker, are already pinned at the
// IdentityService level in identity_test.go, and Registry.Ensure adds no
// logic of its own on either path — it calls IdentityService.Ensure/Inspect
// and returns exactly what they answer.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
)

// newTestUUID is a random v4 UUID, for reconcileWorld's plant fixtures.
func newTestUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// equalStringPtr compares two *string by value, not by address: a plain !=
// on two structs holding *string fields — EnsuredRegistration's
// LabelNamespace, PreviousHost and PreviousLabelNamespace among them —
// compares pointers, which two separate Ensure calls building equal but
// separately allocated strings would fail even when their content matches.
func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equalEnsuredRegistrations compares two results field by field, comparing
// every *string field by value via equalStringPtr instead of by address.
func equalEnsuredRegistrations(a, b *registry.EnsuredRegistration) bool {
	return a.RepositoryID == b.RepositoryID &&
		a.WorktreeID == b.WorktreeID &&
		a.Slug == b.Slug &&
		a.Domain == b.Domain &&
		equalStringPtr(a.LabelNamespace, b.LabelNamespace) &&
		a.Root == b.Root &&
		a.Branch == b.Branch &&
		equalStringPtr(a.PreviousHost, b.PreviousHost) &&
		equalStringPtr(a.PreviousLabelNamespace, b.PreviousLabelNamespace)
}

func TestWorktreeRegistryEnsure(t *testing.T) {
	ctx := context.Background()

	t.Run("uses the complete branch name for new slugs", func(t *testing.T) {
		nested := newRegistryFixture(t, "test/worktrees", acmeDomain)
		deeplyNested := newRegistryFixture(t, "users/alice/feature", acmeDomain)

		got, err := nested.registry.Ensure(ctx, "")
		if err != nil || got.Slug != "test-worktrees" {
			t.Fatalf("got %+v, %v", got, err)
		}
		got, err = deeplyNested.registry.Ensure(ctx, "")
		if err != nil || got.Slug != "users-alice-feature" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("uses the worktree directory when HEAD is detached", func(t *testing.T) {
		f := newRegistryFixture(t, "", acmeDomain)
		got, err := f.registry.Ensure(ctx, "")
		if err != nil || got.Slug != "worktree" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("persists UUID identities and derives stable managed paths", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/ISSUE-123-routing", acmeDomain)

		first, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		f.git.SetBranch(f.root, "renamed/ISSUE-123-routing")
		second, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}

		if first.Slug != "feature-issue-123-routing" || first.Domain != acmeDomain {
			t.Fatalf("unexpected first: %+v", first)
		}
		if second.Slug != first.Slug || second.Domain != acmeDomain ||
			second.RepositoryID != first.RepositoryID || second.WorktreeID != first.WorktreeID ||
			second.Branch != "renamed/ISSUE-123-routing" {
			t.Fatalf("unexpected second: %+v", second)
		}
		if want := registry.WorktreeHome(f.controlHome, first.RepositoryID, first.WorktreeID); want !=
			filepath.Join(f.controlHome, "repositories", first.RepositoryID, "instances", first.WorktreeID) {
			t.Fatalf("unexpected home: %s", want)
		}

		list, err := f.registry.List(ctx)
		if err != nil || len(list) != 1 || list[0].WorktreeID != first.WorktreeID || list[0].Slug != first.Slug {
			t.Fatalf("unexpected list: %+v, %v", list, err)
		}

		data, err := os.ReadFile(filepath.Join(registry.RegistryDirectory(f.controlHome, first.RepositoryID), first.WorktreeID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk map[string]any
		if err := json.Unmarshal(data, &onDisk); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"version": float64(1), "repositoryId": first.RepositoryID, "worktreeId": first.WorktreeID,
			"slug": "feature-issue-123-routing", "domain": acmeDomain, "initialization": "pending",
		}
		for k, v := range want {
			if onDisk[k] != v {
				t.Fatalf("field %s: got %v, want %v (record: %v)", k, onDisk[k], v, onDisk)
			}
		}
	})

	t.Run("reads an existing registration without waiting for the operation lock", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		worktree, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		operation, _ := lock.WorktreeScopes(f.controlHome, worktree.RepositoryID, worktree.WorktreeID, "active build", procid.Markers{})
		held := lock.New(operation, lock.Options{Logger: logger.Silent{}})
		if _, err := held.Acquire(ctx, "active build"); err != nil {
			t.Fatal(err)
		}
		defer held.Release()

		current, err := f.registry.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current == nil || current.Slug != worktree.Slug || current.RepositoryID != worktree.RepositoryID || current.WorktreeID != worktree.WorktreeID {
			t.Fatalf("unexpected current: %+v, %v", current, err)
		}
	})

	t.Run("serializes concurrent first registration", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/concurrent", acmeDomain)
		second := registry.New(registry.Options{ControlHome: f.controlHome, Root: f.root, Domain: acmeDomain, Runner: f.runner, Logger: f.log, UserHome: filepath.Join(f.base, "user-home")})

		var wg sync.WaitGroup
		results := make([]*registry.EnsuredRegistration, 2)
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); results[0], errs[0] = f.registry.Ensure(ctx, "") }()
		go func() { defer wg.Done(); results[1], errs[1] = second.Ensure(ctx, "") }()
		wg.Wait()

		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v, %v", errs[0], errs[1])
		}
		if !equalEnsuredRegistrations(results[0], results[1]) {
			t.Fatalf("expected equal results, got %+v and %+v", results[0], results[1])
		}
		list, err := f.registry.List(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("expected exactly one registration, got %+v, %v", list, err)
		}
	})

	t.Run("adopts an existing home without a registration as completed", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/recovered", acmeDomain)
		worktree, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(registry.RegistryDirectory(f.controlHome, worktree.RepositoryID), worktree.WorktreeID+".json")
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(registry.WorktreeHome(f.controlHome, worktree.RepositoryID, worktree.WorktreeID), 0o700); err != nil {
			t.Fatal(err)
		}

		if _, err := f.registry.Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var onDisk map[string]any
		if err := json.Unmarshal(data, &onDisk); err != nil {
			t.Fatal(err)
		}
		if onDisk["initialization"] != "complete" || onDisk["domain"] != acmeDomain {
			t.Fatalf("unexpected record: %v", onDisk)
		}
	})

	t.Run("does not rebind data when Git reuses a worktree administrative name", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/old", acmeDomain)
		old, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		reusedGitDir := filepath.Join(f.common, "worktrees", "worktree")
		if err := os.RemoveAll(reusedGitDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(reusedGitDir, 0o700); err != nil {
			t.Fatal(err)
		}
		f.git.SetBranch(f.root, "feature/new")

		fresh, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if fresh.WorktreeID == old.WorktreeID {
			t.Fatalf("expected a new worktree identity, got the same one: %s", fresh.WorktreeID)
		}
		if registry.WorktreeHome(f.controlHome, fresh.RepositoryID, fresh.WorktreeID) ==
			registry.WorktreeHome(f.controlHome, old.RepositoryID, old.WorktreeID) {
			t.Fatalf("expected distinct homes")
		}
		records, err := f.registry.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var sawOld, sawFresh bool
		for _, r := range records {
			sawOld = sawOld || r.WorktreeID == old.WorktreeID
			sawFresh = sawFresh || r.WorktreeID == fresh.WorktreeID
		}
		if !sawOld || !sawFresh {
			t.Fatalf("expected both records listed, got %+v", records)
		}

		freshData, err := os.ReadFile(filepath.Join(registry.RegistryDirectory(f.controlHome, fresh.RepositoryID), fresh.WorktreeID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !contains(string(freshData), `"initialization": "pending"`) {
			t.Fatalf("expected the new registration pending, got:\n%s", freshData)
		}

		candidates, err := f.registry.PruneCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var prunable bool
		for _, c := range candidates {
			prunable = prunable || c.WorktreeID == old.WorktreeID
		}
		if !prunable {
			t.Fatalf("expected the old registration to be a prune candidate, got %+v", candidates)
		}
	})

	t.Run("preserves identity when a worktree moves", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/move", acmeDomain)
		first, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		moved := filepath.Join(filepath.Dir(f.root), "moved-worktree")
		if err := os.MkdirAll(moved, 0o700); err != nil {
			t.Fatal(err)
		}
		gitCommonDir, gitDir, ok := f.git.Identity(f.root)
		if !ok {
			t.Fatal("expected an identity for the original root")
		}
		f.git.Forget(f.root)
		f.git.SetActive([]string{moved})
		f.git.SetIdentity(moved, "feature/move", gitCommonDir, gitDir)
		movedRegistry := registry.New(registry.Options{ControlHome: f.controlHome, Root: moved, Domain: acmeDomain, Runner: f.runner, Logger: f.log, UserHome: filepath.Join(f.base, "user-home")})

		second, err := movedRegistry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if second.WorktreeID != first.WorktreeID {
			t.Fatalf("expected the same worktree identity, got %s vs %s", second.WorktreeID, first.WorktreeID)
		}
		if second.Root != moved {
			t.Fatalf("expected root %s, got %s", moved, second.Root)
		}
	})
}

func TestLabelNamespaceInRecord(t *testing.T) {
	ctx := context.Background()

	t.Run("is written on registration, refreshed in place when the config changes it, and absent when never given", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		plain, err := f.registry.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if plain.LabelNamespace != nil {
			t.Fatalf("expected no label namespace yet, got %v", *plain.LabelNamespace)
		}

		adopted := registry.New(registry.Options{ControlHome: f.controlHome, Root: f.root, Domain: acmeDomain, LabelNamespace: "dev.predecessor", Runner: f.runner, Logger: f.log, UserHome: filepath.Join(f.base, "user-home")})
		first, err := adopted.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if first.Slug != plain.Slug || first.LabelNamespace == nil || *first.LabelNamespace != "dev.predecessor" {
			t.Fatalf("unexpected first: %+v", first)
		}
		if first.PreviousHost != nil {
			t.Fatalf("expected no previous host, got %v", *first.PreviousHost)
		}

		own := registry.New(registry.Options{ControlHome: f.controlHome, Root: f.root, Domain: acmeDomain, LabelNamespace: "dev.treerig", Runner: f.runner, Logger: f.log, UserHome: filepath.Join(f.base, "user-home")})
		second, err := own.Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if second.Slug != plain.Slug || second.LabelNamespace == nil || *second.LabelNamespace != "dev.treerig" {
			t.Fatalf("unexpected second: %+v", second)
		}
		list, err := own.List(ctx)
		if err != nil || len(list) != 1 || list[0].LabelNamespace == nil || *list[0].LabelNamespace != "dev.treerig" {
			t.Fatalf("unexpected list: %+v, %v", list, err)
		}
	})
}

// reconcileWorld is several named worktrees sharing one control home, each
// addressable through its own Registry (a fresh domain reads as a different
// config), plus plant for records built directly on disk, bypassing the
// write path.
type reconcileWorld struct {
	t           *testing.T
	base        string
	controlHome string
	git         *registrytest.GitRunner
	runner      run.Runner
	log         *logger.Recording
}

// newReconcileWorld builds a reconcileWorld rooted at a fresh temp directory,
// with its own fake git runner and recording logger.
func newReconcileWorld(t *testing.T) *reconcileWorld {
	t.Helper()
	base := registrytest.TempDir(t)
	runner, git := registrytest.NewFakeRunner()
	return &reconcileWorld{t: t, base: base, controlHome: filepath.Join(base, "control"), git: git, runner: runner, log: &logger.Recording{}}
}

// wt creates a worktree directory named name on branch, with its own git
// common directory, and registers it with the fake git runner. It returns
// the worktree's root.
func (w *reconcileWorld) wt(name, branch string) string {
	w.t.Helper()
	root := filepath.Join(w.base, name)
	common := filepath.Join(w.base, name+"-common")
	if err := os.MkdirAll(root, 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "worktrees", name), 0o700); err != nil {
		w.t.Fatal(err)
	}
	w.git.Add(root, branch, common)
	return root
}

// reg opens a Registry for root under this world's control home and domain,
// sharing its fake runner and recording logger.
func (w *reconcileWorld) reg(root, domain string) *registry.Registry {
	return registry.New(registry.Options{
		ControlHome: w.controlHome, Root: root, Domain: domain, Runner: w.runner, Logger: w.log,
		UserHome: filepath.Join(w.base, "user-home"),
	})
}

// plant writes a fixture registry record directly to disk, bypassing the
// registry lock, for a record from "another repository" the ensure()-under-
// test never created.
func (w *reconcileWorld) plant(repositoryID, worktreeID, slug, domain, initialization string) {
	w.t.Helper()
	dir := registry.RegistryDirectory(w.controlHome, repositoryID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"version": 1, "repositoryId": repositoryID, "worktreeId": worktreeID,
		"slug": slug, "domain": domain, "initialization": initialization,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, worktreeID+".json"), data, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

const (
	oldDomain = "acme.localhost"
	newDomain = "acme.dev"
)

var suffixedSlug = regexp.MustCompile(`^feature-login-[a-f0-9]{12}$`)

func TestDomainReconciliationInEnsure(t *testing.T) {
	ctx := context.Background()

	t.Run("re-registers under the new domain, keeps the slug when it is free, preserves initialization, reports the move", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "feature/login")
		before, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		after, err := w.reg(a, newDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if after.WorktreeID != before.WorktreeID || after.Slug != "feature-login" || after.Domain != newDomain ||
			after.PreviousHost == nil || *after.PreviousHost != "feature-login."+oldDomain {
			t.Fatalf("unexpected after: %+v", after)
		}
		data, err := os.ReadFile(filepath.Join(registry.RegistryDirectory(w.controlHome, before.RepositoryID), before.WorktreeID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk map[string]any
		if err := json.Unmarshal(data, &onDisk); err != nil {
			t.Fatal(err)
		}
		if onDisk["domain"] != newDomain || onDisk["initialization"] != "pending" {
			t.Fatalf("unexpected on-disk record: %v", onDisk)
		}
		lines, _ := w.log.Snapshot()
		if len(lines) != 1 || !contains(lines[0], "moved from feature-login."+oldDomain+" to feature-login."+newDomain) {
			t.Fatalf("unexpected log lines: %v", lines)
		}
		again, err := w.reg(a, newDomain).Ensure(ctx, "")
		if err != nil || again.PreviousHost != nil {
			t.Fatalf("expected no previous host on a stable re-ensure, got %+v, %v", again, err)
		}
		all, err := w.reg(a, newDomain).ListAll(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("expected exactly one registration, got %+v, %v", all, err)
		}
	})

	t.Run("takes a suffix when a newer worktree already owns the bare slug in the new domain", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "feature/login")
		if _, err := w.reg(a, oldDomain).Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}
		b := w.wt("b", "feature/login")
		bEnsured, err := w.reg(b, newDomain).Ensure(ctx, "")
		if err != nil || bEnsured.Slug != "feature-login" {
			t.Fatalf("unexpected b: %+v, %v", bEnsured, err)
		}
		reconciled, err := w.reg(a, newDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if !suffixedSlug.MatchString(reconciled.Slug) || reconciled.Domain != newDomain {
			t.Fatalf("expected a suffixed slug under the new domain, got %+v", reconciled)
		}
		scan, err := w.reg(a, newDomain).ScanAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Conflicts) != 0 {
			t.Fatalf("expected no conflicts, got %v", scan.Conflicts)
		}
	})

	t.Run("serializes a rename against a concurrent first registration; hostnames stay unique", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "feature/login")
		if _, err := w.reg(a, oldDomain).Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}
		c := w.wt("c", "feature/login")

		var wg sync.WaitGroup
		results := make([]*registry.EnsuredRegistration, 2)
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); results[0], errs[0] = w.reg(a, newDomain).Ensure(ctx, "") }()
		go func() { defer wg.Done(); results[1], errs[1] = w.reg(c, newDomain).Ensure(ctx, "") }()
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v, %v", errs[0], errs[1])
		}
		ra, rc := results[0], results[1]
		if ra.Slug == rc.Slug {
			t.Fatalf("expected distinct slugs, got %s twice", ra.Slug)
		}
		suffixedCount := 0
		for _, slug := range []string{ra.Slug, rc.Slug} {
			if suffixedSlug.MatchString(slug) {
				suffixedCount++
			}
		}
		if suffixedCount != 1 {
			t.Fatalf("expected exactly one suffixed slug, got %s and %s", ra.Slug, rc.Slug)
		}
		scan, err := w.reg(a, newDomain).ScanAll(ctx)
		if err != nil || len(scan.Conflicts) != 0 {
			t.Fatalf("expected no conflicts, got %v, %v", scan.Conflicts, err)
		}
	})

	t.Run("flips back deterministically when the config reverts", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "feature/login")
		first, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.reg(a, newDomain).Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}
		back, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if back.Slug != first.Slug || back.Domain != oldDomain || back.PreviousHost == nil || *back.PreviousHost != "feature-login."+newDomain {
			t.Fatalf("unexpected back: %+v", back)
		}
	})

	t.Run("current() warns on a mismatch and does not mutate", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "feature/login")
		before, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		seen, err := w.reg(a, newDomain).Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if seen == nil || seen.Slug != before.Slug || seen.Domain != oldDomain {
			t.Fatalf("unexpected seen: %+v, %v", seen, err)
		}
		lines, _ := w.log.Snapshot()
		if len(lines) != 1 || !contains(lines[0], "now says "+newDomain) {
			t.Fatalf("unexpected log lines: %v", lines)
		}
		again, err := w.reg(a, oldDomain).Current(ctx)
		if err != nil || again == nil || again.Domain != oldDomain {
			t.Fatalf("unexpected again: %+v, %v", again, err)
		}
		lines, _ = w.log.Snapshot()
		if len(lines) != 1 {
			t.Fatalf("expected no further warnings, got %v", lines)
		}
	})
}

func TestScanFailureModesInEnsure(t *testing.T) {
	ctx := context.Background()

	t.Run("an unreadable record does not brick reads or registered worktrees, but blocks new allocation", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "main")
		registered, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		futureRepo := newTestUUID()
		dir := registry.RegistryDirectory(w.controlHome, futureRepo)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{
			"version": 2, "repositoryId": futureRepo, "worktreeId": newTestUUID(),
			"slug": "x", "domain": "y.localhost", "initialization": "complete", "extra": true,
		})
		if err := os.WriteFile(filepath.Join(dir, newTestUUID()+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		again, err := w.reg(a, oldDomain).Ensure(ctx, "")
		if err != nil || again.Slug != registered.Slug {
			t.Fatalf("unexpected again: %+v, %v", again, err)
		}
		current, err := w.reg(a, oldDomain).Current(ctx)
		if err != nil || current == nil || current.Slug != registered.Slug {
			t.Fatalf("unexpected current: %+v, %v", current, err)
		}
		all, err := w.reg(a, oldDomain).ListAll(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("expected one readable registration, got %+v, %v", all, err)
		}
		found, err := w.reg(a, oldDomain).Resolve(ctx, registered.WorktreeID, registry.NameOptions{})
		if err != nil || found == nil || found.Record.Slug != "main" {
			t.Fatalf("unexpected Resolve: %+v, %v", found, err)
		}
		scan, err := w.reg(a, oldDomain).ScanAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Problems) != 1 || !contains(scan.Problems[0].File, futureRepo) {
			t.Fatalf("unexpected problems: %+v", scan.Problems)
		}

		fresh := w.wt("fresh", "feature/new")
		if _, err := w.reg(fresh, oldDomain).Ensure(ctx, ""); err == nil ||
			!contains(err.Error(), "Refusing to allocate a hostname while the registry has unreadable records") {
			t.Fatalf("expected the unreadable-records refusal, got %v", err)
		}
	})

	t.Run("a symlinked record is reported as a problem, not silently skipped, and blocks new allocation", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "main")
		if _, err := w.reg(a, oldDomain).Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}
		repo, id := newTestUUID(), newTestUUID()
		dir := registry.RegistryDirectory(w.controlHome, repo)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		elsewhere := filepath.Join(w.base, "elsewhere.json")
		data, _ := json.Marshal(map[string]any{
			"version": 1, "repositoryId": repo, "worktreeId": id, "slug": "linked", "domain": oldDomain, "initialization": "complete",
		})
		if err := os.WriteFile(elsewhere, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(dir, id+".json")); err != nil {
			t.Fatal(err)
		}

		scan, err := w.reg(a, oldDomain).ScanAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Problems) != 1 || !contains(scan.Problems[0].Reason, "symbolic link") {
			t.Fatalf("unexpected problems: %+v", scan.Problems)
		}
		for _, record := range scan.Records {
			if record.Slug == "linked" {
				t.Fatalf("expected the symlinked record to be excluded, got %+v", scan.Records)
			}
		}

		fresh := w.wt("fresh", "feature/new")
		if _, err := w.reg(fresh, oldDomain).Ensure(ctx, ""); err == nil || !contains(err.Error(), "unreadable records") {
			t.Fatalf("expected the unreadable-records refusal, got %v", err)
		}
	})

	t.Run("a duplicate hostname is reported, still occupies its host, and only fails lookups that land on it", func(t *testing.T) {
		w := newReconcileWorld(t)
		a := w.wt("a", "main")
		reg := w.reg(a, oldDomain)
		if _, err := reg.Ensure(ctx, ""); err != nil {
			t.Fatal(err)
		}
		repo, x, y := newTestUUID(), newTestUUID(), newTestUUID()
		w.plant(repo, x, "feature-login", oldDomain, "complete")
		w.plant(repo, y, "feature-login", oldDomain, "complete")

		all, err := reg.ListAll(ctx)
		if err != nil || len(all) != 3 {
			t.Fatalf("expected three registrations, got %+v, %v", all, err)
		}
		scan, err := reg.ScanAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := scan.Conflicts["feature-login."+oldDomain]; !ok || len(scan.Conflicts) != 1 {
			t.Fatalf("expected exactly one conflict on feature-login.%s, got %v", oldDomain, scan.Conflicts)
		}
		if found, err := reg.Resolve(ctx, x, registry.NameOptions{}); err != nil || found == nil || found.Record.WorktreeID != x {
			t.Fatalf("unexpected Resolve by UUID: %+v, %v", found, err)
		}
		if _, err := reg.Resolve(ctx, "feature-login", registry.NameOptions{Domain: oldDomain}); err == nil || !contains(err.Error(), "names more than one worktree") {
			t.Fatalf("expected an ambiguous-hostname refusal, got %v", err)
		}
		if _, err := reg.Resolve(ctx, "feature-login."+oldDomain, registry.NameOptions{}); err == nil || !contains(err.Error(), "names more than one worktree") {
			t.Fatalf("expected an ambiguous-hostname refusal when the host is named, got %v", err)
		}
		if found, err := reg.Resolve(ctx, "main", registry.NameOptions{Domain: oldDomain}); err != nil || found == nil || found.Record.Slug != "main" {
			t.Fatalf("unexpected Resolve for an unambiguous slug: %+v, %v", found, err)
		}
		other := w.wt("other", "feature/login")
		otherEnsured, err := w.reg(other, oldDomain).Ensure(ctx, "")
		if err != nil || !suffixedSlug.MatchString(otherEnsured.Slug) {
			t.Fatalf("expected a suffixed slug for a colliding branch, got %+v, %v", otherEnsured, err)
		}
		unrelated := w.wt("unrelated", "feature/other")
		unrelatedEnsured, err := w.reg(unrelated, oldDomain).Ensure(ctx, "")
		if err != nil || unrelatedEnsured.Slug != "feature-other" {
			t.Fatalf("expected an unaffected allocation, got %+v, %v", unrelatedEnsured, err)
		}
	})
}
