package overview

// Which repositories a Snapshot asks Git about (#204): the one the service runs
// in through its Identities, any other through a directory its cards recorded,
// each within a bound; and the reads a person's `list` asks for.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// newGitFake is a gitFake for a repository whose one worktree is a fresh
// directory; hang makes its Git stuck for good, until the test is over.
func newGitFake(t *testing.T, hang bool) *gitFake {
	t.Helper()
	git := &gitFake{dockerFake: &dockerFake{}, root: registrytest.TempDir(t), hang: hang, release: make(chan struct{})}
	t.Cleanup(func() { close(git.release) })
	return git
}

// gitFake answers the Git calls of IdentityService.Entries for a repository
// whose one worktree is at root and carries no treerig identity, or never
// answers them, and passes every other call to the Docker double.
type gitFake struct {
	*dockerFake
	root string
	// hang makes every Git call wait for its context to end.
	hang bool
	// release ends the calls hang holds; the test closes it when it is over.
	release chan struct{}
	// onGit, when set, runs as each Git call starts.
	onGit func()

	mu     sync.Mutex
	listed []string
}

// Run answers Git's calls, and Docker's through the embedded double.
func (g *gitFake) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if spec.Command != "git" {
		return g.dockerFake.Run(ctx, spec)
	}
	g.mu.Lock()
	if strings.Contains(strings.Join(spec.Args, " "), "worktree list") {
		for i, arg := range spec.Args {
			if arg == "-C" && i+1 < len(spec.Args) {
				g.listed = append(g.listed, spec.Args[i+1])
			}
		}
	}
	g.mu.Unlock()
	if g.onGit != nil {
		g.onGit()
	}
	if g.hang {
		// Not the context: a call stuck on a dead mount does not hear it.
		<-g.release
		return run.Result{}, errors.New("released")
	}
	args := strings.Join(spec.Args, " ")
	switch {
	case strings.Contains(args, "worktree list"):
		return run.Result{Stdout: "worktree " + g.root + "\x00HEAD 0000000000000000000000000000000000000000\x00branch refs/heads/main\x00\x00"}, nil
	case strings.Contains(args, "--show-toplevel"):
		return run.Result{Stdout: g.root + "\n"}, nil
	case strings.Contains(args, "branch --show-current"):
		return run.Result{Stdout: "main\n"}, nil
	case strings.Contains(args, "--git-dir"), strings.Contains(args, "--git-common-dir"):
		return run.Result{Stdout: filepath.Join(g.root, ".git") + "\n"}, nil
	}
	return run.Result{}, nil
}

// otherRepository is a harness with the three testRegistrations of the current
// repository and the fourth, of another one, whose card records root.
func otherRepository(t *testing.T, git *gitFake) (*serviceHarness, registry.RegisteredWorktree) {
	t.Helper()
	regs := testRegistrations()
	h := newServiceHarness(t, git.dockerFake, regs, nil)
	h.service.opts.Runner = git
	h.service.opts.Identities = &fakeIdentities{repositoryID: testRepo, repositoryIDPresent: true}
	foreign := regs[3]
	for name, id := range map[string]string{"repository-id": foreign.RepositoryID, "worktree-id": foreign.WorktreeID} {
		marker := filepath.Join(git.root, ".git", "treerig", name)
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte(registry.IdentityMarkerContents(id)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(card.IndexCardPath(h.controlHome, foreign.RepositoryID, foreign.WorktreeID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := card.WriteIndexCard(h.controlHome, foreign.RepositoryID, foreign.WorktreeID, card.IndexCard{
		Project: "widgets", Domain: foreign.Domain, Slug: foreign.Slug, Host: foreign.Slug + "." + foreign.Domain,
		Root: git.root, Branch: "main", LabelNamespace: "dev.treerig", Services: []card.IndexService{}, UpdatedAt: "2026-09-21T10:00:00.000Z",
	}); err != nil {
		t.Fatal(err)
	}
	return h, foreign
}

func TestSnapshotAsksOtherRepositories(t *testing.T) {
	t.Run("lists the worktrees of another repository through a directory its card recorded, once", func(t *testing.T) {
		git := newGitFake(t, false)
		h, foreign := otherRepository(t, git)

		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(git.listed) != 1 || git.listed[0] != git.root {
			t.Fatalf("Git was asked to list %v, want %s once", git.listed, git.root)
		}
		entry := findBySlug(snapshot.Entries, foreign.Slug)
		if entry == nil || entry.State == worktree.StateUnknown {
			t.Fatalf("entry = %+v", entry)
		}
	})

	t.Run("asks about none of them for one repository's snapshot", func(t *testing.T) {
		git := newGitFake(t, false)
		h, _ := otherRepository(t, git)

		snapshot, err := h.service.SnapshotOf(context.Background(), testRepo)
		if err != nil {
			t.Fatal(err)
		}
		if len(git.listed) != 0 {
			t.Fatalf("Git was asked to list %v", git.listed)
		}
		for _, entry := range snapshot.Entries {
			if entry.RepositoryID != testRepo {
				t.Fatalf("entry of another repository: %+v", entry)
			}
		}
		if len(snapshot.Entries) != 3 {
			t.Fatalf("entries = %+v, want the three of the repository", snapshot.Entries)
		}
	})

	t.Run("shows a repository that does not answer in time as unknown, and does not wait on it for long", func(t *testing.T) {
		git := newGitFake(t, true)
		h, foreign := otherRepository(t, git)
		h.service.opts.GitTimeout = 50 * time.Millisecond
		h.service.opts.WarnOfRepositories = true

		start := time.Now()
		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %s", elapsed)
		}
		entry := findBySlug(snapshot.Entries, foreign.Slug)
		if entry == nil || entry.State != worktree.StateUnknown || !strings.Contains(entry.Hint, "Git did not answer within 50ms") {
			t.Fatalf("entry = %+v", entry)
		}
		if len(snapshot.GitHints) != 1 || snapshot.GitHints[0] != entry.Hint {
			t.Fatalf("GitHints = %q, want the entry's hint, to be said once", snapshot.GitHints)
		}
		if !containsSubstring(snapshotLines(h.logger), "Git could not list the worktrees of 1 repository, so their states show unknown") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("does not warn of a repository that does not answer unless asked to, as a refresh after a command is not", func(t *testing.T) {
		git := newGitFake(t, true)
		h, foreign := otherRepository(t, git)
		h.service.opts.GitTimeout = 50 * time.Millisecond

		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if entry := findBySlug(snapshot.Entries, foreign.Slug); entry == nil || entry.State != worktree.StateUnknown {
			t.Fatalf("entry = %+v", entry)
		}
		if containsSubstring(snapshotLines(h.logger), "Git could not list") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("holds neither the current repository's Git nor another's to more than the one bound", func(t *testing.T) {
		git := newGitFake(t, true)
		h, _ := otherRepository(t, git)
		h.service.opts.GitTimeout = 50 * time.Millisecond
		stuck := &stuckIdentities{release: git.release}
		h.service.opts.Identities = stuck

		start := time.Now()
		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %s", elapsed)
		}
		if snapshot.IdentityError == nil || !strings.Contains(snapshot.IdentityError.Reason, "Git did not answer within 50ms") {
			t.Fatalf("identityError = %+v", snapshot.IdentityError)
		}
	})

	t.Run("does not ask about a repository no card has a directory for", func(t *testing.T) {
		git := newGitFake(t, false)
		h, foreign := otherRepository(t, git)
		if err := os.Remove(card.IndexCardPath(h.controlHome, foreign.RepositoryID, foreign.WorktreeID)); err != nil {
			t.Fatal(err)
		}

		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(git.listed) != 0 {
			t.Fatalf("Git was asked to list %v", git.listed)
		}
		if entry := findBySlug(snapshot.Entries, foreign.Slug); entry == nil || entry.State != worktree.StateStopped {
			t.Fatalf("entry = %+v, want its history's state", entry)
		}
	})

	t.Run("shows a repository whose recorded directory now belongs to another as unknown", func(t *testing.T) {
		git := newGitFake(t, false)
		h, foreign := otherRepository(t, git)
		// The directory the card names has become somebody else's.
		marker := filepath.Join(git.root, ".git", "treerig", "repository-id")
		if err := os.WriteFile(marker, []byte(registry.IdentityMarkerContents(testRepo)), 0o600); err != nil {
			t.Fatal(err)
		}

		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		entry := findBySlug(snapshot.Entries, foreign.Slug)
		if entry == nil || entry.State != worktree.StateUnknown || !strings.Contains(entry.Hint, "belong to another repository") {
			t.Fatalf("entry = %+v", entry)
		}
	})

	t.Run("passes on a cancellation", func(t *testing.T) {
		git := newGitFake(t, true)
		h, _ := otherRepository(t, git)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		git.onGit = cancel
		if _, err := h.service.Snapshot(ctx); err == nil {
			t.Fatal("a cancelled Snapshot returned a snapshot")
		}
	})
}

// stuckIdentities is an Identities whose Git never answers, whatever its
// context says.
type stuckIdentities struct{ release chan struct{} }

// Repository never answers before the test is over.
func (s *stuckIdentities) Repository(context.Context, string) (string, bool, bool, error) {
	<-s.release
	return "", false, false, errors.New("released")
}

// Entries never answers before the test is over.
func (s *stuckIdentities) Entries(context.Context) ([]registry.WorktreeEntry, error) {
	<-s.release
	return nil, errors.New("released")
}

func TestSnapshotOfNothing(t *testing.T) {
	t.Run("asks Docker and Git nothing when there is nothing to list, and says so with the service's clock", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, nil, nil)

		snapshot, err := h.service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.DockerReachable || snapshot.DockerError != nil || len(docker.Calls()) != 0 || snapshot.Entries == nil {
			t.Fatalf("snapshot = %+v, docker calls = %v", snapshot, docker.Calls())
		}
		if snapshot.GeneratedAt != "2026-09-21T12:00:00.000Z" || h.service.EmptySnapshot().GeneratedAt != snapshot.GeneratedAt {
			t.Fatalf("generatedAt = %q, want the service's clock", snapshot.GeneratedAt)
		}
	})
}

func TestRefreshWarnsOfDocker(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		docker := &dockerFake{exit: 1}
		h := newServiceHarness(t, docker, testRegistrations()[:1], nil)
		h.service.opts.QuietDocker = quiet

		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		warned := containsSubstring(snapshotLines(h.logger), "The overview cannot show what runs because containers could not be listed")
		if warned == quiet {
			t.Fatalf("quiet = %v, warned = %v, lines = %v", quiet, warned, snapshotLines(h.logger))
		}
	}
}

func TestRefreshTakesNoLockWhileItAsksGit(t *testing.T) {
	git := newGitFake(t, true)
	h, _ := otherRepository(t, git)
	h.service.opts.GitTimeout = 300 * time.Millisecond
	h.service.opts.LockTimeout = time.Second

	done := make(chan error, 1)
	go func() {
		_, err := h.service.Refresh(context.Background())
		done <- err
	}()
	// While Git is being waited on, the router's lock is anyone's.
	other := lock.New(h.service.opts.Lock, lock.Options{Timeout: 100 * time.Millisecond, Holder: "someone else", Logger: logger.Silent{}})
	if _, err := other.Acquire(context.Background(), "someone else"); err != nil {
		t.Fatalf("the lock was held while Git was asked: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A refresh reads before it takes the lock, so one that read earlier can reach
// the lock after one that read later has written. The newer page stays.
func TestRefreshKeepsANewerPage(t *testing.T) {
	h := newServiceHarness(t, &dockerFake{}, testRegistrations()[:1], nil)
	at := func(clock time.Time) { h.service.opts.Now = func() time.Time { return clock } }
	read := func() string {
		page, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(page)
	}
	later := time.Date(2026, 9, 21, 12, 0, 5, 0, time.UTC)
	earlier := later.Add(-4 * time.Second)

	// The refresh that read at 12:00:05 finishes first; the one that read at
	// 12:00:01 finishes after it.
	at(later)
	if _, err := h.service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	newer := read()
	at(earlier)
	snapshot, err := h.service.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if read() != newer || !strings.Contains(newer, `<time datetime="2026-09-21T12:00:05.000Z"`) {
		t.Fatalf("the older refresh overwrote the newer page:\n%s", read())
	}
	if snapshot.GeneratedAt != "2026-09-21T12:00:01.000Z" {
		t.Fatalf("snapshot = %q: the caller still gets what it read", snapshot.GeneratedAt)
	}

	// A later one replaces it, and so does one over a page that says nothing.
	at(later.Add(time.Second))
	if _, err := h.service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), `<time datetime="2026-09-21T12:00:06.000Z"`) {
		t.Fatalf("a newer refresh did not write:\n%s", read())
	}
	if err := os.WriteFile(h.service.opts.IndexFile, []byte("not a page"), 0o644); err != nil {
		t.Fatal(err)
	}
	at(earlier)
	if _, err := h.service.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), `<time datetime="2026-09-21T12:00:01.000Z"`) {
		t.Fatalf("a page that says nothing was not replaced:\n%s", read())
	}
}
