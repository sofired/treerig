package worktreestate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// repository is a worktreestate.Repository double.
type repository struct {
	entries []registry.WorktreeEntry
	err     error
}

// Entries answers what the double holds.
func (r repository) Entries(context.Context) ([]registry.WorktreeEntry, error) {
	return r.entries, r.err
}

// world is a control home with registrations, and a Gatherer over a docker
// that answers with whatever lines the test sets.
type world struct {
	t           *testing.T
	controlHome string
	dir         string
	docker      *dockerFake
	mounts      func() (worktreestate.Mounts, error)
	mountReads  atomic.Int32
}

// newWorld is an empty control home, a docker that names no containers, and a
// mount table that lists only the root.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, controlHome: registrytest.TempDir(t), dir: registrytest.TempDir(t), docker: &dockerFake{byFilter: map[string]string{}}}
	w.mounts = func() (worktreestate.Mounts, error) { return worktreestate.Mounts{"/": "ext4"}, nil }
	return w
}

// register writes a registration with the given initialization and returns it.
func (w *world) register(worktreeID, slug, initialization string) registry.RegisteredWorktree {
	w.t.Helper()
	if _, err := registrytest.WriteRecord(w.controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repoA, WorktreeID: worktreeID, Slug: slug, Domain: "acme.localhost", Initialization: initialization,
	}); err != nil {
		w.t.Fatal(err)
	}
	return registry.RegisteredWorktree{RepositoryID: repoA, WorktreeID: worktreeID, Slug: slug, Domain: "acme.localhost"}
}

// gatherer is a Gatherer over the world, counting its reads of the mount table.
func (w *world) gatherer() *worktreestate.Gatherer {
	return worktreestate.New(worktreestate.Options{
		ControlHome: w.controlHome, Runner: w.docker.runner(),
		Mounts: func() (worktreestate.Mounts, error) { w.mountReads.Add(1); return w.mounts() },
	})
}

// gather classifies subjects, asking Docker about the default namespace and Git about local.
func (w *world) gather(repositories map[string]worktreestate.Repository, subjects ...worktreestate.Subject) worktreestate.Report {
	w.t.Helper()
	report, err := w.gatherer().Gather(context.Background(), worktreestate.Request{
		Subjects: subjects, Namespaces: []string{"dev.treerig"}, Repositories: repositories,
	})
	if err != nil {
		w.t.Fatalf("Gather: %v", err)
	}
	return report
}

// containers sets the lines `docker ps` answers for the default namespace.
func (w *world) containers(lines ...string) {
	w.docker.byFilter["label=dev.treerig.managed=true"] = strings.Join(lines, "\n")
}

// stateOf is the state report holds for record, failing the test when it holds none.
func stateOf(t *testing.T, report worktreestate.Report, record registry.RegisteredWorktree) worktreestate.Result {
	t.Helper()
	result, ok := report.States[worktreestate.Key(record.RepositoryID, record.WorktreeID)]
	if !ok {
		t.Fatalf("no state for %s in %+v", record.Slug, report.States)
	}
	return result
}

func TestGatherFromContainersAndHistory(t *testing.T) {
	t.Run("tells stopped from not started by the registration's history, not by whether containers exist", func(t *testing.T) {
		w := newWorld(t)
		finished := w.register(wtA, "finished", "complete")
		fresh := w.register(wtB, "fresh", "pending")
		w.containers()
		report := w.gather(nil,
			worktreestate.Subject{Registration: finished, Declared: []string{"web"}},
			worktreestate.Subject{Registration: fresh, Declared: []string{"web"}})
		if got := stateOf(t, report, finished).State; got != worktree.StateStopped {
			t.Errorf("after an up finished and down removed the containers: %q", got)
		}
		if got := stateOf(t, report, fresh).State; got != worktree.StateNotStarted {
			t.Errorf("registered, no up ever finished: %q", got)
		}
	})

	t.Run("lets the containers decide over history", func(t *testing.T) {
		w := newWorld(t)
		fresh := w.register(wtA, "fresh", "pending")
		w.containers(
			line(repoA, wtA, "web", "running", "Up 1 minute"),
			line(repoA, wtA, "worker", "exited", "Exited (3) 1 minute ago"),
		)
		report := w.gather(nil, worktreestate.Subject{Registration: fresh, Declared: []string{"web", "worker"}})
		if got := stateOf(t, report, fresh).State; got != worktree.StatePartlyRunning {
			t.Errorf("a first up that failed with one service running: %q", got)
		}
	})

	t.Run("counts a service that finished cleanly as having done its job", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(
			line(repoA, wtA, "web", "running", "Up 1 minute"),
			line(repoA, wtA, "migrate", "exited", "Exited (0) 1 minute ago"),
		)
		report := w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"web", "migrate"}})
		if got := stateOf(t, report, app).State; got != worktree.StateRunning {
			t.Errorf("web running beside a finished migrate: %q", got)
		}
	})

	t.Run("is not running while a declared service has no container, or one replica of a scaled service is down", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(line(repoA, wtA, "api", "running", "Up 1 minute"))
		report := w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"api", "db"}})
		if got := stateOf(t, report, app).State; got != worktree.StatePartlyRunning {
			t.Errorf("a declared service with no container: %q", got)
		}
		w.containers(
			line(repoA, wtA, "api", "running", "Up 1 minute"),
			line(repoA, wtA, "api", "exited", "Exited (1) 1 minute ago"),
		)
		report = w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"api"}})
		if got := stateOf(t, report, app).State; got != worktree.StatePartlyRunning {
			t.Errorf("one replica of two down: %q, want partly running, not failed", got)
		}
	})

	t.Run("does not take a shared container for any worktree's service", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(strings.Join([]string{"running", "Up 1 minute", repoA, "", "api", "true", "s1"}, "\t"))
		report := w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"api"}})
		if got := stateOf(t, report, app).State; got != worktree.StateStopped {
			t.Errorf("got %q", got)
		}
	})

	t.Run("is unknown, naming Docker, when Docker did not answer", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.docker.exit, w.docker.stderr = 1, "Cannot connect to the Docker daemon\n"
		report := w.gather(nil, worktreestate.Subject{Registration: app})
		got := stateOf(t, report, app)
		if got.State != worktree.StateUnknown || !strings.HasPrefix(got.Hint, "Docker ") {
			t.Fatalf("got %+v", got)
		}
		if report.Docker.Answered || report.Docker.Error == "" {
			t.Fatalf("docker = %+v", report.Docker)
		}
	})

	t.Run("is unknown when a service is in a state treerig has no word for", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(line(repoA, wtA, "web", "frozen", "Frozen"))
		got := stateOf(t, w.gather(nil, worktreestate.Subject{Registration: app}), app)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "web is in a state treerig has no word for") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown rather than guessed from containers when the card could not be read", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(line(repoA, wtA, "web", "running", "Up 1 minute"))
		got := stateOf(t, w.gather(nil, worktreestate.Subject{Registration: app, Unreadable: "too many open files"}), app)
		if got.State != worktree.StateUnknown || got.Hint != "too many open files" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, naming the file, when the registration cannot be read as it was", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		app.Slug = "renamed"
		got := stateOf(t, w.gather(nil, worktreestate.Subject{Registration: app}), app)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "no longer owns registration") {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestGatherRemoving(t *testing.T) {
	t.Run("reports a worktree whose removal started as removing, whatever its containers say, until the marker is gone", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		w.containers(line(repoA, wtA, "web", "running", "Up 1 minute"))
		marker := filepath.Join(w.controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		if err := os.WriteFile(marker, []byte(registry.RemovalMarkerContents()), 0o600); err != nil {
			t.Fatal(err)
		}
		report := w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"web"}})
		if got := stateOf(t, report, app).State; got != worktree.StateRemoving {
			t.Fatalf("got %q", got)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		report = w.gather(nil, worktreestate.Subject{Registration: app, Declared: []string{"web"}})
		if got := stateOf(t, report, app).State; got != worktree.StateRunning {
			t.Fatalf("with the marker gone: %q", got)
		}
	})

	t.Run("refuses to read a marker of a newer format as a removal, and says so", func(t *testing.T) {
		w := newWorld(t)
		app := w.register(wtA, "app", "complete")
		marker := filepath.Join(w.controlHome, "repositories", repoA, "worktrees", wtA+".removing")
		if err := os.WriteFile(marker, []byte("format 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := stateOf(t, w.gather(nil, worktreestate.Subject{Registration: app}), app)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "written by a newer treerig") {
			t.Fatalf("got %+v", got)
		}
	})
}

// entry is a Git worktree entry for the local repository.
func entry(id, root string, mutate func(*registry.WorktreeEntry)) registry.WorktreeEntry {
	e := registry.WorktreeEntry{
		ListedWorktree: registry.ListedWorktree{Root: root, Branch: "feature"},
		Exists:         true, RepositoryID: repoA, WorktreeID: id,
	}
	if mutate != nil {
		mutate(&e)
	}
	return e
}

func TestGatherDirectory(t *testing.T) {
	const hintParent = "Its parent directory is missing or empty: is the volume mounted?"
	local := func(entries ...registry.WorktreeEntry) map[string]worktreestate.Repository {
		return map[string]worktreestate.Repository{repoA: repository{entries: entries}}
	}

	// gone is a prunable entry whose directory is not there, under a parent
	// directory the test fills (or not).
	setup := func(t *testing.T) (w *world, record registry.RegisteredWorktree, parent, root string) {
		w = newWorld(t)
		record = w.register(wtA, "old-spike", "complete")
		parent = filepath.Join(w.dir, "src")
		if err := os.MkdirAll(filepath.Join(parent, "sibling"), 0o700); err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(parent, "old-spike")
		w.containers()
		return w, record, parent, root
	}
	gone := func(root string) registry.WorktreeEntry {
		return entry(wtA, root, func(e *registry.WorktreeEntry) {
			e.Exists, e.Prunable = false, true
			e.PrunableReason = "gitdir file points to non-existent location"
		})
	}

	t.Run("calls the directory missing on positive evidence: prunable, not locked, absent, and a parent that is there and not empty", func(t *testing.T) {
		w, record, _, root := setup(t)
		got := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateDirectoryMissing || got.Hint != "" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("reads the mount table once for the whole run", func(t *testing.T) {
		w, record, parent, root := setup(t)
		other := w.register(wtB, "other", "complete")
		otherRoot := filepath.Join(parent, "other")
		report := w.gather(local(gone(root), entry(wtB, otherRoot, func(e *registry.WorktreeEntry) { e.Exists, e.Prunable = false, true })),
			worktreestate.Subject{Registration: record}, worktreestate.Subject{Registration: other})
		if stateOf(t, report, other).State != worktree.StateDirectoryMissing || stateOf(t, report, record).State != worktree.StateDirectoryMissing {
			t.Fatalf("states = %+v", report.States)
		}
		if reads := w.mountReads.Load(); reads != 1 {
			t.Fatalf("the mount table was read %d times, want once", reads)
		}
	})

	t.Run("is unknown, asking whether the volume is mounted, when the parent is missing or empty", func(t *testing.T) {
		w, record, parent, root := setup(t)
		if err := os.RemoveAll(filepath.Join(parent, "sibling")); err != nil {
			t.Fatal(err)
		}
		got := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != hintParent {
			t.Fatalf("empty parent: %+v", got)
		}
		if err := os.Remove(parent); err != nil {
			t.Fatal(err)
		}
		got = stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != hintParent {
			t.Fatalf("missing parent: %+v", got)
		}
	})

	t.Run("is unknown when the parent is below an automount placeholder", func(t *testing.T) {
		w, record, parent, root := setup(t)
		w.mounts = func() (worktreestate.Mounts, error) {
			return worktreestate.Mounts{"/": "ext4", filepath.Dir(parent): "autofs"}, nil
		}
		got := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != hintParent {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, with the error, when the mount table cannot be read", func(t *testing.T) {
		w, record, _, root := setup(t)
		w.mounts = func() (worktreestate.Mounts, error) { return nil, errors.New("no /proc here") }
		got := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != "treerig couldn't check it: no /proc here." {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, not missing, for a directory that is there but that Git calls prunable", func(t *testing.T) {
		w, record, _, root := setup(t)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		damaged := entry(wtA, root, func(e *registry.WorktreeEntry) { e.Prunable = true })
		got := stateOf(t, w.gather(local(damaged), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != "Git's records for it are damaged: 'git worktree repair' may fix them." {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, not missing, for a locked entry whose directory is absent", func(t *testing.T) {
		w, record, _, root := setup(t)
		locked := entry(wtA, root, func(e *registry.WorktreeEntry) { e.Exists, e.Locked = false, true })
		got := stateOf(t, w.gather(local(locked), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "locked") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown when Git does not call an absent directory prunable", func(t *testing.T) {
		w, record, _, root := setup(t)
		old := entry(wtA, root, func(e *registry.WorktreeEntry) { e.Exists = false })
		got := stateOf(t, w.gather(local(old), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "prunable") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, with the error, when the directory cannot be checked", func(t *testing.T) {
		w, record, _, root := setup(t)
		denied := entry(wtA, root, func(e *registry.WorktreeEntry) { e.Exists, e.StatErr = false, os.ErrPermission })
		got := stateOf(t, w.gather(local(denied), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != "treerig couldn't check it: permission denied." {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is unknown, with Git's words, for every worktree of the repository when Git did not answer", func(t *testing.T) {
		w, record, _, _ := setup(t)
		foreign := registry.RegisteredWorktree{RepositoryID: "22222222-2222-4222-8222-222222222222", WorktreeID: wtB, Slug: "elsewhere", Domain: "acme.localhost"}
		if _, err := registrytest.WriteRecord(w.controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: foreign.RepositoryID, WorktreeID: foreign.WorktreeID, Slug: foreign.Slug, Domain: foreign.Domain, Initialization: "complete",
		}); err != nil {
			t.Fatal(err)
		}
		broken := map[string]worktreestate.Repository{repoA: repository{err: errors.New("fatal: detected dubious ownership")}}
		report := w.gather(broken, worktreestate.Subject{Registration: record}, worktreestate.Subject{Registration: foreign})
		if got := stateOf(t, report, record); got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "dubious ownership") {
			t.Fatalf("local: %+v", got)
		}
		if got := stateOf(t, report, foreign); got.State != worktree.StateStopped {
			t.Fatalf("another repository's worktree is not Git's to answer for: %+v", got)
		}
		if !strings.Contains(report.GitError, "dubious ownership") {
			t.Fatalf("GitError = %q", report.GitError)
		}
	})

	t.Run("asks no one about a worktree of another repository", func(t *testing.T) {
		w, _, _, _ := setup(t)
		foreign := registry.RegisteredWorktree{RepositoryID: "22222222-2222-4222-8222-222222222222", WorktreeID: wtB, Slug: "elsewhere", Domain: "acme.localhost"}
		if _, err := registrytest.WriteRecord(w.controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: foreign.RepositoryID, WorktreeID: foreign.WorktreeID, Slug: foreign.Slug, Domain: foreign.Domain, Initialization: "pending",
		}); err != nil {
			t.Fatal(err)
		}
		report := w.gather(local(), worktreestate.Subject{Registration: foreign, RecordedRoot: "/nowhere/at/all"})
		if got := stateOf(t, report, foreign).State; got != worktree.StateNotStarted {
			t.Fatalf("got %q", got)
		}
		if w.mountReads.Load() != 0 {
			t.Fatal("the mount table was read for nothing")
		}
	})

	t.Run("checks a worktree Git no longer lists by the directory the last up recorded", func(t *testing.T) {
		w, record, parent, root := setup(t)
		got := stateOf(t, w.gather(local(), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if got.State != worktree.StateDirectoryMissing {
			t.Fatalf("pruned, parent fine: %+v", got)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		got = stateOf(t, w.gather(local(), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "git worktree repair") {
			t.Fatalf("pruned but the directory is there: %+v", got)
		}
		got = stateOf(t, w.gather(local(), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateUnknown || got.Hint != "Git does not list it and no directory was recorded for it: run 'treerig worktree remove old-spike' if it is gone." {
			t.Fatalf("pruned, nothing recorded: %+v", got)
		}
		if err := os.RemoveAll(parent); err != nil {
			t.Fatal(err)
		}
		got = stateOf(t, w.gather(local(), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if got.State != worktree.StateUnknown || got.Hint != hintParent {
			t.Fatalf("pruned, parent gone: %+v", got)
		}
	})

	t.Run("says nothing about an unlisted worktree while another entry might be it", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			others  func(root string) []registry.WorktreeEntry
			wantErr string
		}{
			{"a worktree whose identity could not be read", func(root string) []registry.WorktreeEntry {
				return []registry.WorktreeEntry{entry("", filepath.Join(filepath.Dir(root), "moved"), func(e *registry.WorktreeEntry) {
					e.RepositoryID, e.IdentityErr = "", errors.New("format 2, written by a newer treerig")
				})}
			}, "newer treerig"},
			{"a worktree that could not be examined", func(root string) []registry.WorktreeEntry {
				return []registry.WorktreeEntry{entry(wtB, filepath.Join(filepath.Dir(root), "elsewhere"), func(e *registry.WorktreeEntry) {
					e.Exists, e.StatErr = false, os.ErrPermission
				})}
			}, "permission denied"},
			{"a worktree treerig has no identity for, somewhere else, whose directory is gone or damaged: this one, moved", func(root string) []registry.WorktreeEntry {
				return []registry.WorktreeEntry{entry("", filepath.Join(filepath.Dir(root), "moved"), func(e *registry.WorktreeEntry) {
					e.RepositoryID, e.Exists, e.Prunable = "", false, true
				})}
			}, "might be this one moved"},
			{"a damaged directory Git calls prunable, with no identity", func(root string) []registry.WorktreeEntry {
				return []registry.WorktreeEntry{entry("", filepath.Join(filepath.Dir(root), "damaged"), func(e *registry.WorktreeEntry) { e.RepositoryID, e.Prunable = "", true })}
			}, "might be this one moved"},
		} {
			t.Run(c.name, func(t *testing.T) {
				w, record, _, root := setup(t)
				got := stateOf(t, w.gather(local(c.others(root)...), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
				if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, c.wantErr) {
					t.Fatalf("got %+v, want unknown mentioning %q", got, c.wantErr)
				}
			})
		}

		t.Run("a live checkout treerig has no identity for, such as a plain git worktree add or the main worktree, is not this one", func(t *testing.T) {
			w, record, _, root := setup(t)
			live := []registry.WorktreeEntry{
				entry("", filepath.Join(filepath.Dir(root), "unmanaged"), func(e *registry.WorktreeEntry) { e.RepositoryID = "" }),
				entry("", filepath.Dir(filepath.Dir(root)), func(e *registry.WorktreeEntry) { e.RepositoryID = "" }),
			}
			got := stateOf(t, w.gather(local(live...), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
			if got.State != worktree.StateDirectoryMissing || got.Basis != worktreestate.BasisRecordedRoot {
				t.Fatalf("got %+v, want directory missing on the recorded directory alone", got)
			}
		})

		t.Run("an entry that carries another worktree's identity is nothing to do with it", func(t *testing.T) {
			w, record, _, root := setup(t)
			others := []registry.WorktreeEntry{entry(wtB, filepath.Join(filepath.Dir(root), "elsewhere"), nil)}
			got := stateOf(t, w.gather(local(others...), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
			if got.State != worktree.StateDirectoryMissing {
				t.Fatalf("got %+v", got)
			}
		})
	})

	t.Run("records how the directory was judged, so a caller can demand the identity", func(t *testing.T) {
		w, record, _, root := setup(t)
		byUUID := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if byUUID.State != worktree.StateDirectoryMissing || byUUID.Basis != worktreestate.BasisUUID {
			t.Fatalf("by identity: %+v", byUUID)
		}
		byRoot := stateOf(t, w.gather(local(), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if byRoot.State != worktree.StateDirectoryMissing || byRoot.Basis != worktreestate.BasisRecordedRoot {
			t.Fatalf("by recorded directory: %+v", byRoot)
		}
		unidentified := entry("", root, func(e *registry.WorktreeEntry) { e.RepositoryID, e.Exists, e.Prunable = "", false, true })
		byMatchedRoot := stateOf(t, w.gather(local(unidentified), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if byMatchedRoot.State != worktree.StateDirectoryMissing || byMatchedRoot.Basis != worktreestate.BasisRecordedRoot {
			t.Fatalf("by an entry at the recorded directory: %+v", byMatchedRoot)
		}
		foreign := w.register(wtB, "foreign", "complete")
		nobody := stateOf(t, w.gather(nil, worktreestate.Subject{Registration: foreign}), foreign)
		if nobody.Basis != worktreestate.BasisNone {
			t.Fatalf("a repository nobody asked Git about has no basis: %+v", nobody)
		}
	})

	t.Run("finds a worktree whose marker cannot be read by its recorded directory, and says what is wrong", func(t *testing.T) {
		w, record, _, root := setup(t)
		broken := entry("", root, func(e *registry.WorktreeEntry) {
			e.RepositoryID, e.IdentityErr = "", errors.New("format 2, written by a newer treerig")
		})
		got := stateOf(t, w.gather(local(broken), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "newer treerig") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("does not take another worktree now living in the recorded directory for this one", func(t *testing.T) {
		w, record, _, root := setup(t)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		occupant := entry(wtB, root, nil)
		got := stateOf(t, w.gather(local(occupant), worktreestate.Subject{Registration: record, RecordedRoot: root}), record)
		if got.State != worktree.StateUnknown || !strings.Contains(got.Hint, "git worktree repair") {
			t.Fatalf("got %+v: the registration's own directory has someone else in it, which is not Present", got)
		}
	})

	t.Run("lets the removal marker and the evidence outrank unknown, in that order", func(t *testing.T) {
		w, record, _, root := setup(t)
		w.docker.exit = 1
		got := stateOf(t, w.gather(local(gone(root)), worktreestate.Subject{Registration: record}), record)
		if got.State != worktree.StateDirectoryMissing {
			t.Fatalf("with Docker down and the directory gone: %+v", got)
		}
	})
}

func TestCurrent(t *testing.T) {
	w := newWorld(t)
	finished := w.register(wtA, "finished", "complete")
	fresh := w.register(wtB, "fresh", "pending")
	g := w.gatherer()
	for _, c := range []struct {
		name     string
		record   registry.RegisteredWorktree
		services []worktree.ServiceStatus
		hint     string
		want     worktree.State
	}{
		{"services running", finished, []worktree.ServiceStatus{{Service: "web", State: worktree.Running}}, "", worktree.StateRunning},
		{"every service not created, after an up", finished, []worktree.ServiceStatus{{Service: "web", State: worktree.NotCreated}}, "", worktree.StateStopped},
		{"every service not created, no up yet", fresh, []worktree.ServiceStatus{{Service: "web", State: worktree.NotCreated}}, "", worktree.StateNotStarted},
		{"a crashed service", fresh, []worktree.ServiceStatus{{Service: "web", State: worktree.Exited, ExitCode: 3}}, "", worktree.StateFailed},
		{"Docker not answering", finished, nil, "Compose couldn't read this worktree: bad file", worktree.StateUnknown},
	} {
		t.Run("classifies "+c.name, func(t *testing.T) {
			if got := g.Current(c.record, c.services, worktree.DirectoryPresent, c.hint).State; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDockerHint(t *testing.T) {
	for reason, want := range map[string]string{
		"":                                    "Docker didn't answer.",
		"Docker is not available: no socket":  "Docker is not available: no socket",
		"`docker ps` didn't answer within 5s": "`docker ps` didn't answer within 5s",
		"Error response from daemon: something broke": "Docker didn't answer: Error response from daemon: something broke",
	} {
		if got := worktreestate.DockerHint(reason); got != want {
			t.Errorf("DockerHint(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestSubjectOf(t *testing.T) {
	record := registry.RegisteredWorktree{RepositoryID: repoA, WorktreeID: wtA, Slug: "app", Domain: "acme.localhost"}

	t.Run("declares every service the card names, whatever its URL, and the directory it saw", func(t *testing.T) {
		indexCard := &card.IndexCard{Root: "/work/app", Services: []card.IndexService{
			{Name: "api", URL: "http://app.acme.localhost"}, {Name: "worker", URL: ""}, {Name: "odd", URL: "http://elsewhere.example/"},
		}}
		subject := worktreestate.SubjectOf(record, indexCard, nil)
		if !reflect.DeepEqual(subject.Declared, []string{"api", "worker", "odd"}) || subject.RecordedRoot != "/work/app" || subject.Unreadable != "" {
			t.Fatalf("subject = %+v", subject)
		}
	})

	t.Run("says the card could not be read, and declares nothing", func(t *testing.T) {
		subject := worktreestate.SubjectOf(record, nil, errors.New("too many open files"))
		if subject.Unreadable != "Its index card couldn't be read: too many open files." || subject.Declared != nil {
			t.Fatalf("subject = %+v", subject)
		}
	})

	t.Run("has nothing to say for a card never written", func(t *testing.T) {
		if subject := worktreestate.SubjectOf(record, nil, nil); subject.Declared != nil || subject.RecordedRoot != "" || subject.Unreadable != "" {
			t.Fatalf("subject = %+v", subject)
		}
	})
}

func TestAnsweredRepository(t *testing.T) {
	want := errors.New("boom")
	entries, err := worktreestate.Answered{Listed: []registry.WorktreeEntry{{}}, Err: want}.Entries(context.Background())
	if len(entries) != 1 || err != want {
		t.Fatalf("Entries = %v, %v", entries, err)
	}
}

func TestRecordedDirectory(t *testing.T) {
	g := worktreestate.New(worktreestate.Options{
		Mounts: func() (worktreestate.Mounts, error) { return worktreestate.Mounts{"/": "ext4"}, nil },
	})
	parent := registrytest.TempDir(t)
	if err := os.Mkdir(filepath.Join(parent, "sibling"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("a directory that is there is present", func(t *testing.T) {
		if got, hint := g.RecordedDirectory(filepath.Join(parent, "sibling")); got != worktree.DirectoryPresent || hint != "" {
			t.Fatalf("got %v, %q", got, hint)
		}
	})

	t.Run("an absent one beside other files is gone", func(t *testing.T) {
		if got, hint := g.RecordedDirectory(filepath.Join(parent, "gone")); got != worktree.DirectoryGone || hint != "" {
			t.Fatalf("got %v, %q", got, hint)
		}
	})

	t.Run("one whose parent is missing or empty may be an unmounted volume", func(t *testing.T) {
		empty := filepath.Join(parent, "mountpoint")
		if err := os.Mkdir(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(empty, "gone"), filepath.Join(parent, "nowhere", "gone")} {
			got, hint := g.RecordedDirectory(path)
			if got != worktree.DirectoryUndetermined || !strings.Contains(hint, "is the volume mounted?") {
				t.Fatalf("%s: got %v, %q", path, got, hint)
			}
		}
	})

	t.Run("one below an automount placeholder, or with an unreadable mount table, is undetermined", func(t *testing.T) {
		auto := worktreestate.New(worktreestate.Options{
			Mounts: func() (worktreestate.Mounts, error) { return worktreestate.Mounts{parent: "autofs"}, nil },
		})
		if got, _ := auto.RecordedDirectory(filepath.Join(parent, "gone")); got != worktree.DirectoryUndetermined {
			t.Fatalf("automount: got %v", got)
		}
		broken := worktreestate.New(worktreestate.Options{
			Mounts: func() (worktreestate.Mounts, error) { return nil, errors.New("no /proc here") },
		})
		if got, hint := broken.RecordedDirectory(filepath.Join(parent, "gone")); got != worktree.DirectoryUndetermined || !strings.Contains(hint, "no /proc here") {
			t.Fatalf("mount table: got %v, %q", got, hint)
		}
	})
}
