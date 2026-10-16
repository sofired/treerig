package registry_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

func TestValidateSlug(t *testing.T) {
	t.Run("accepts DNS labels", func(t *testing.T) {
		for _, slug := range []string{"a", "login", "feature-login", "9lives", "a-b-c", strings.Repeat("a", 63)} {
			if err := registry.ValidateSlug(slug); err != nil {
				t.Errorf("ValidateSlug(%q) = %v", slug, err)
			}
		}
	})

	t.Run("refuses everything else", func(t *testing.T) {
		for _, slug := range []string{"", "shared", "Login", "a.b", "-a", "a-", "a_b", "a b", "ü", strings.Repeat("a", 64)} {
			if err := registry.ValidateSlug(slug); err == nil {
				t.Errorf("ValidateSlug(%q) accepted it", slug)
			}
		}
	})
}

func TestStoreRename(t *testing.T) {
	ctx := context.Background()

	t.Run("replaces the slug and keeps every other field, running migrate with the old and new records first", func(t *testing.T) {
		controlHome := claimedHome(t)
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: repoA, WorktreeID: wtA, Slug: "feature-login", Domain: acmeDomain,
			Initialization: "complete", Ports: map[string]int{"api": 30001},
		}); err != nil {
			t.Fatal(err)
		}
		store := registry.NewStore(controlHome, repoA)
		var seen [2]string

		renamed, err := store.Rename(ctx, wtA, "feature-login", "login", testWriter, func(previous, renamed registry.RegisteredWorktree) error {
			seen = [2]string{previous.Slug, renamed.Slug}
			// The record still names the old slug while migrate runs.
			if now, _ := store.Find(wtA); now.Slug != "feature-login" {
				t.Errorf("record already renamed inside migrate: %s", now.Slug)
			}
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}

		if seen != [2]string{"feature-login", "login"} || renamed.Slug != "login" {
			t.Fatalf("seen = %v, renamed = %+v", seen, renamed)
		}
		found, _ := store.Find(wtA)
		if found.Slug != "login" || found.Domain != acmeDomain || found.Ports["api"] != 30001 {
			t.Fatalf("record = %+v", found)
		}
		if state, _ := store.Initialization(wtA, "login"); state != registry.Complete {
			t.Fatalf("initialization = %s", state)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("leaves the old slug registered when migrate fails", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)
		boom := errors.New("boom")

		_, err := store.Rename(ctx, wtA, "feature-login", "login", testWriter, func(_, _ registry.RegisteredWorktree) error { return boom }, nil)

		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if found, _ := store.Find(wtA); found.Slug != "feature-login" {
			t.Fatalf("slug = %s", found.Slug)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("refuses a slug taken in the same domain by a worktree of another repository, and takes it in another domain", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoB, wtB, "login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		_, err := store.Rename(ctx, wtA, "feature-login", "login", testWriter, nil, nil)
		if err == nil || err.Error() != `The slug "login" is held in `+acmeDomain+` by `+wtB+`. Choose another slug.` {
			t.Fatalf("err = %v", err)
		}

		writeMinimalRecord(t, controlHome, repoB, wtB, "login", "widgets.localhost", "complete")
		if _, err := store.Rename(ctx, wtA, "feature-login", "login", testWriter, nil, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a record that now names another slug", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "already-renamed", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		_, err := store.Rename(ctx, wtA, "feature-login", "login", testWriter, nil, nil)

		if err == nil || !strings.Contains(err.Error(), "no longer owns registration") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("renaming to the held slug neither migrates nor writes", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)

		_, err := store.Rename(ctx, wtA, "login", "login", testWriter, func(_, _ registry.RegisteredWorktree) error {
			t.Error("migrate ran")
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses the reserved slug without reading the registry", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		if _, err := store.Rename(ctx, wtA, "feature-login", "shared", testWriter, nil, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("names the holder of a taken slug with the describe function when it answers", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoB, wtB, "login", acmeDomain, "complete")

		_, err := registry.NewStore(controlHome, repoA).Rename(ctx, wtA, "feature-login", "login", testWriter, nil,
			func(registry.RegisteredWorktree) string { return "/checkouts/login" })

		if err == nil || err.Error() != `The slug "login" is held in `+acmeDomain+` by /checkouts/login. Choose another slug.` {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a second rename to the same slug waits out the first, then is refused: the check and the write share one registry hold", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "one", acmeDomain, "complete")
		writeMinimalRecord(t, controlHome, repoB, wtB, "two", acmeDomain, "complete")
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 2)
		go func() {
			// Parks inside the migration, which runs under the registry lock.
			_, err := registry.NewStore(controlHome, repoA).Rename(ctx, wtA, "one", "contested", testWriter,
				func(_, _ registry.RegisteredWorktree) error {
					close(entered)
					<-release
					return nil
				}, nil)
			done <- err
		}()
		<-entered
		go func() {
			_, err := registry.NewStore(controlHome, repoB).Rename(ctx, wtB, "two", "contested", testWriter, nil, nil)
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("a rename finished while the first held the registry lock: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		first, second := <-done, <-done
		if (first == nil) == (second == nil) {
			t.Fatalf("results = %v, %v: want exactly one success", first, second)
		}
		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil || len(scan.Conflicts) != 0 {
			t.Fatalf("conflicts = %v, %v", scan.Conflicts, err)
		}
	})

	t.Run("refuses while an unreadable record might hold the hostname", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		bad := registry.RegistryDirectory(controlHome, repoB)
		if err := os.MkdirAll(bad, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bad+"/"+wtB+".json", []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := registry.NewStore(controlHome, repoA).Rename(ctx, wtA, "feature-login", "login", testWriter, nil, nil)

		if err == nil || !strings.Contains(err.Error(), "unreadable records") {
			t.Fatalf("err = %v", err)
		}
	})
}
