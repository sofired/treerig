package registry_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// newerMessage is the refusal a store gives for state a newer treerig wrote
// (docs/design/state.md), for subject, which is "the ..." plus a path.
func newerMessage(subject, found string, supported int) string {
	return "Refusing to use " + subject + ": it is format " + found + ", written by a newer treerig, and this build reads format " +
		string(rune('0'+supported)) + ". Upgrade treerig, then run this again."
}

// TestIdentityMarkerFormat covers the version in the identity markers: written
// as a format line before the UUID, a newer one refused with the upgrade
// instruction, and a marker without the line treated as not one treerig wrote.
func TestIdentityMarkerFormat(t *testing.T) {
	ctx := context.Background()
	const id = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"

	t.Run("writes both markers as the format line, then the UUID", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/format")
		identity, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		pattern := regexp.MustCompile(`^format 1\n[0-9a-f-]{36}\n$`)
		for path, want := range map[string]string{
			filepath.Join(identity.GitCommonDir, "treerig", "repository-id"): identity.RepositoryID,
			filepath.Join(identity.GitDir, "treerig", "worktree-id"):         identity.WorktreeID,
		} {
			data, err := os.ReadFile(path)
			if err != nil || !pattern.Match(data) || !strings.HasSuffix(string(data), want+"\n") {
				t.Fatalf("%s = %q, %v", path, data, err)
			}
		}
	})

	t.Run("IdentityMarkerContents is the format line and the UUID", func(t *testing.T) {
		if got := registry.IdentityMarkerContents(id); got != "format 1\n"+id+"\n" {
			t.Fatalf("got %q", got)
		}
	})

	for _, which := range []string{"repository-id", "worktree-id"} {
		t.Run("refuses a "+which+" marker a newer treerig wrote, leaving it as it is", func(t *testing.T) {
			f := newIdentityFixture(t, "feature/newer")
			identity, err := f.identities.Ensure(ctx, f.root)
			if err != nil {
				t.Fatal(err)
			}
			dir := identity.GitDir
			if which == "repository-id" {
				dir = identity.GitCommonDir
			}
			marker := filepath.Join(dir, "treerig", which)
			// A newer layout is judged by its first line alone.
			newer := "format 2\n{\"id\": \"" + id + "\"}\n"
			if err := os.WriteFile(marker, []byte(newer), 0o600); err != nil {
				t.Fatal(err)
			}
			want := newerMessage("the identity marker "+marker, "2", 1)
			for name, call := range map[string]func() error{
				"Ensure":  func() error { _, err := f.identities.Ensure(ctx, f.root); return err },
				"Inspect": func() error { _, err := f.identities.Inspect(ctx, f.root, registry.InspectOptions{}); return err },
				"RepositoryID": func() error {
					_, _, err := f.identities.RepositoryID(ctx, f.root)
					return err
				},
			} {
				if which == "worktree-id" && name == "RepositoryID" {
					continue
				}
				if err := call(); err == nil || err.Error() != want {
					t.Errorf("%s: got %v\nwant %s", name, err, want)
				}
			}
			if data, _ := os.ReadFile(marker); string(data) != newer {
				t.Fatalf("marker rewritten to %q", data)
			}
		})
	}

	t.Run("refuses to remove a worktree marker a newer treerig wrote", func(t *testing.T) {
		f := newIdentityFixture(t, "feature/newer-remove")
		identity, err := f.identities.Ensure(ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(identity.GitDir, "treerig", "worktree-id")
		if err := os.WriteFile(marker, []byte("format 9\n"+id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err = f.identities.RemoveWorktreeMarker(*identity)
		if err == nil || err.Error() != newerMessage("the identity marker "+marker, "9", 1) {
			t.Fatalf("got %v", err)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Fatalf("the marker was removed: %v", statErr)
		}
	})

	for name, contents := range map[string]string{
		"a bare UUID, with no format line":    id + "\n",
		"an empty file":                       "",
		"a format line and nothing else":      "format 1\n",
		"format 0":                            "format 0\n" + id + "\n",
		"a format line with a trailing space": "format 1 \n" + id + "\n",
		"a format line that is not the first": id + "\nformat 1\n",
		"the UUID followed by another line":   "format 1\n" + id + "\nextra\n",
		"a format line and a non-UUID":        "format 1\nnot-a-uuid\n",
	} {
		t.Run("treats "+name+" as an invalid marker, not a newer one", func(t *testing.T) {
			f := newIdentityFixture(t, "feature/foreign")
			identity, err := f.identities.Ensure(ctx, f.root)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(identity.GitDir, "treerig", "worktree-id")
			if err := os.WriteFile(marker, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = f.identities.Inspect(ctx, f.root, registry.InspectOptions{})
			want := "Invalid identity marker: " + marker + ". A marker is the line \"format 1\" and then the checkout's UUID. " +
				"To start over, move the file aside: the checkout then gets a new identity, and its old registration is reported by `doctor` for `worktree remove` to clear."
			if err == nil || err.Error() != want {
				t.Fatalf("got %v", err)
			}
			if data, _ := os.ReadFile(marker); string(data) != contents {
				t.Fatalf("marker rewritten to %q", data)
			}
		})
	}
}

// TestRegistrationFormat covers the version in a registry record: written by
// every writer, a newer one refused by readers and writers alike with the
// upgrade instruction, and a missing or unknown one reported as not a record.
func TestRegistrationFormat(t *testing.T) {
	newerRecord := func(version string) string {
		return `{"version":` + version + `,"registrations":[{"id":"` + wtA + `"}],"somethingNew":true}`
	}
	write := func(t *testing.T, controlHome, raw string) string {
		t.Helper()
		file := filepath.Join(registry.RegistryDirectory(controlHome, repoA), wtA+".json")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}

	t.Run("a record every writer touches says version 1", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		store := registry.NewStore(controlHome, repoA)
		if err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil || !strings.HasPrefix(string(data), "{\n  \"version\": 1,\n") {
			t.Fatalf("record = %q, %v", data, err)
		}
	})

	t.Run("Find, Initialization and Scan refuse a newer record, whatever else it holds, naming the file", func(t *testing.T) {
		controlHome := claimedHome(t)
		file := write(t, controlHome, newerRecord("2"))
		store := registry.NewStore(controlHome, repoA)
		want := newerMessage("the registration "+file, "2", 1)

		if _, err := store.Find(wtA); err == nil || err.Error() != want {
			t.Errorf("Find: got %v\nwant %s", err, want)
		}
		if _, err := store.Initialization(wtA, "feature-login"); err == nil || err.Error() != want {
			t.Errorf("Initialization: got %v", err)
		}
		records, problems, err := store.Scan()
		if err != nil || len(records) != 0 || len(problems) != 1 || problems[0].Reason != want || problems[0].File != file || !problems[0].Newer {
			t.Errorf("Scan = %+v, %+v, %v", records, problems, err)
		}
	})

	t.Run("a writer refuses a newer record and leaves it as it is", func(t *testing.T) {
		controlHome := claimedHome(t)
		file := write(t, controlHome, newerRecord("3"))
		store := registry.NewStore(controlHome, repoA)
		err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter)
		if err == nil || err.Error() != newerMessage("the registration "+file, "3", 1) {
			t.Fatalf("got %v", err)
		}
		if data, _ := os.ReadFile(file); string(data) != newerRecord("3") {
			t.Fatalf("record rewritten to %q", data)
		}
		requireRegistryFree(t, controlHome)
		if _, err := store.Remove(context.Background(), wtA, "feature-login", testWriter); err == nil {
			t.Fatal("Remove deleted a record this build cannot read")
		}
		if _, statErr := os.Stat(file); statErr != nil {
			t.Fatalf("the record was removed: %v", statErr)
		}
	})

	for name, raw := range map[string]string{
		"no version":       `{"repositoryId":"` + repoA + `","worktreeId":"` + wtA + `","slug":"feature-login","domain":"acme.localhost","initialization":"complete"}`,
		"version 0":        `{"version":0,"repositoryId":"` + repoA + `","worktreeId":"` + wtA + `","slug":"feature-login","domain":"acme.localhost","initialization":"complete"}`,
		"a string version": `{"version":"2","repositoryId":"` + repoA + `"}`,
		"a fractional one": `{"version":1.5}`,
		"a negative one":   `{"version":-1}`,
	} {
		t.Run("reports "+name+" as an unrecognized record, never a newer one", func(t *testing.T) {
			controlHome := claimedHome(t)
			write(t, controlHome, raw)
			_, problems, err := registry.NewStore(controlHome, repoA).Scan()
			if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "unrecognized version or shape") || problems[0].Newer {
				t.Fatalf("problems = %+v, %v", problems, err)
			}
		})
	}
}

// TestSharedRecordFormat is TestRegistrationFormat for a shared service's record.
func TestSharedRecordFormat(t *testing.T) {
	const newer = `{"version":2,"name":"mailpit","domain":"acme.localhost","ports":{"1025":30000},"somethingNew":[1]}`

	t.Run("writes version 1", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		syncShared(t, store, "mailpit", []int{1025}, nil)
		data, err := os.ReadFile(filepath.Join(registry.SharedDirectory(controlHome, repoA), "mailpit.json"))
		if err != nil || !strings.HasPrefix(string(data), "{\n  \"version\": 1,\n") {
			t.Fatalf("record = %q, %v", data, err)
		}
	})

	t.Run("refuses a newer record on read and on write, naming the file and leaving it as it is", func(t *testing.T) {
		controlHome := claimedHome(t)
		file := writeSharedFile(t, controlHome, repoA, "mailpit", newer)
		store := registry.NewStore(controlHome, repoA)
		want := newerMessage("the shared-service record "+file, "2", 1)

		if _, err := store.FindShared("mailpit"); err == nil || err.Error() != want {
			t.Errorf("FindShared: got %v\nwant %s", err, want)
		}
		records, problems, err := store.SharedRecords()
		if err != nil || len(records) != 0 || len(problems) != 1 || problems[0].Reason != want || !problems[0].Newer {
			t.Errorf("SharedRecords = %+v, %+v, %v", records, problems, err)
		}
		_, err = store.SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
			Name: "mailpit", Domain: "acme.localhost", Ports: []int{1025}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
		})
		if err == nil {
			t.Error("SyncSharedPorts wrote over a record this build cannot read")
		}
		if data, _ := os.ReadFile(file); string(data) != newer {
			t.Errorf("record rewritten to %q", data)
		}
	})

	t.Run("reports a record with no version as unrecognized, never a newer one", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeSharedFile(t, controlHome, repoA, "mailpit", `{"name":"mailpit","domain":"acme.localhost","ports":{"1025":30000}}`)
		_, problems, err := registry.NewStore(controlHome, repoA).SharedRecords()
		if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "unrecognized version or shape") || problems[0].Newer {
			t.Fatalf("problems = %+v, %v", problems, err)
		}
	})
}

// TestMovingAnIdentityMarkerAside pins what the invalid-marker message
// promises: a checkout whose worktree marker is moved aside gets a new
// identity, and its old registration is then a prune candidate, which is what
// `doctor` reports under missing checkouts for `worktree remove` to clear.
func TestMovingAnIdentityMarkerAside(t *testing.T) {
	ctx := context.Background()
	f := newRegistryFixture(t, "feature/aside", acmeDomain)
	old, err := f.registry.Ensure(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.registry.Identities.Inspect(ctx, f.root, registry.InspectOptions{})
	if err != nil || identity == nil {
		t.Fatalf("Inspect: %v, %v", identity, err)
	}
	marker := filepath.Join(identity.GitDir, "treerig", "worktree-id")
	if err := os.Rename(marker, marker+".aside"); err != nil {
		t.Fatal(err)
	}

	fresh, err := f.registry.Ensure(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.WorktreeID == old.WorktreeID {
		t.Fatal("the checkout kept its identity")
	}
	candidates, err := f.registry.PruneCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, candidate := range candidates {
		reported = reported || candidate.WorktreeID == old.WorktreeID
	}
	if !reported {
		t.Fatalf("the old registration is not a prune candidate: %+v", candidates)
	}
}
