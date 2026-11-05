package worktreestate_test

// AskRepositories and Within (#204): which directory Git is run in, whether its
// listing is of the repository asked about, and that nothing a hung mount does
// holds the caller past its bound.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktreestate"
)

// gitDirs is a run.Runner that answers the Git calls of
// IdentityService.Entries for worktrees that are directories on disk, with
// identity markers it wrote, and can make a directory's Git never answer: the
// call waits for release, whatever its context says, as a process stuck on a
// dead mount does.
type gitDirs struct {
	release chan struct{}
	hung    map[string]bool

	mu     sync.Mutex
	listed []string
}

// newGitDirs is a gitDirs whose hung calls end with the test.
func newGitDirs(t *testing.T) *gitDirs {
	t.Helper()
	g := &gitDirs{release: make(chan struct{}), hung: map[string]bool{}}
	t.Cleanup(func() { close(g.release) })
	return g
}

// repositoryAt makes a directory that is the one worktree of repository id,
// registered as worktreeOf(id), and returns it.
func repositoryAt(t *testing.T, id string) string {
	t.Helper()
	root := registrytest.TempDir(t)
	for name, contents := range map[string]string{"repository-id": id, "worktree-id": worktreeOf(id)} {
		path := filepath.Join(root, ".git", "treerig", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(registry.IdentityMarkerContents(contents)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// uuid is the n-th identity, spelled as the markers need it.
func uuid(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000000", n) }

// worktreeOf is the identity of the one worktree of the repository with identity id.
func worktreeOf(id string) string { return "ffffffff" + id[8:] }

// Run answers git, for the directory after -C.
func (g *gitDirs) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	var dir string
	for i, arg := range spec.Args {
		if arg == "-C" && i+1 < len(spec.Args) {
			dir = spec.Args[i+1]
		}
	}
	args := strings.Join(spec.Args, " ")
	g.mu.Lock()
	hung := g.hung[dir]
	if strings.Contains(args, "worktree list") {
		g.listed = append(g.listed, dir)
	}
	g.mu.Unlock()
	if hung {
		<-g.release
		return run.Result{}, errors.New("released")
	}
	switch {
	case strings.Contains(args, "worktree list"):
		return run.Result{Stdout: "worktree " + dir + "\x00HEAD 0000\x00branch refs/heads/main\x00\x00"}, nil
	case strings.Contains(args, "--show-toplevel"):
		return run.Result{Stdout: dir + "\n"}, nil
	case strings.Contains(args, "branch --show-current"):
		return run.Result{Stdout: "main\n"}, nil
	case strings.Contains(args, "--git-dir"), strings.Contains(args, "--git-common-dir"):
		return run.Result{Stdout: filepath.Join(dir, ".git") + "\n"}, nil
	}
	return run.Result{}, nil
}

// Which says git is there.
func (g *gitDirs) Which(string) bool { return true }

// hang makes the Git of dir never answer.
func (g *gitDirs) hang(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hung[dir] = true
}

// gatherer is a Gatherer over g with the given bound.
func gathererOver(g run.Runner, budget time.Duration) *worktreestate.Gatherer {
	return worktreestate.New(worktreestate.Options{Runner: g, GitTimeout: budget})
}

func TestAskRepositories(t *testing.T) {
	ctx := context.Background()

	t.Run("lists a repository through the directory its worktrees were last in", func(t *testing.T) {
		git := newGitDirs(t)
		root := repositoryAt(t, uuid(1))
		answers, err := gathererOver(git, time.Second).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{root}}})
		if err != nil || len(answers) != 1 || !answers[0].Asked || answers[0].Err != nil || len(answers[0].Entries) != 1 {
			t.Fatalf("answers = %+v, %v", answers, err)
		}
		if answers[0].Entries[0].RepositoryID != uuid(1) || answers[0].Entries[0].Root != root {
			t.Fatalf("entry = %+v", answers[0].Entries[0])
		}
	})

	t.Run("does not ask about a repository with no recorded directory", func(t *testing.T) {
		git := newGitDirs(t)
		answers, err := gathererOver(git, time.Second).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1)}})
		if err != nil || answers[0].Asked || answers[0].Err != nil || len(git.listed) != 0 {
			t.Fatalf("answers = %+v, %v, listed %v", answers, err, git.listed)
		}
	})

	t.Run("answers an empty listing when no recorded directory is there, a relative one never being used", func(t *testing.T) {
		git := newGitDirs(t)
		gone := filepath.Join(registrytest.TempDir(t), "gone")
		answers, err := gathererOver(git, time.Second).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{gone, "relative/dir", "."}}})
		if err != nil || !answers[0].Asked || answers[0].Err != nil || answers[0].Entries != nil || len(git.listed) != 0 {
			t.Fatalf("answers = %+v, %v, listed %v", answers, err, git.listed)
		}
	})

	t.Run("passes over a directory that now belongs to another repository", func(t *testing.T) {
		git := newGitDirs(t)
		foreign, mine := repositoryAt(t, uuid(99)), repositoryAt(t, uuid(1))
		answers, err := gathererOver(git, time.Second).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{foreign, mine}}})
		if err != nil || answers[0].Err != nil || len(answers[0].Entries) != 1 || answers[0].Entries[0].RepositoryID != uuid(1) {
			t.Fatalf("answers = %+v, %v", answers, err)
		}
		if !slices.Equal(git.listed, []string{foreign, mine}) {
			t.Fatalf("Git was run in %v, want the foreign one first and then the repository's own", git.listed)
		}
	})

	t.Run("says when every directory belongs to another repository, and does not take its worktrees for the repository's", func(t *testing.T) {
		git := newGitDirs(t)
		foreign := repositoryAt(t, uuid(99))
		answers, err := gathererOver(git, time.Second).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{foreign}}})
		if err != nil || answers[0].Err == nil || answers[0].Entries != nil || !strings.Contains(answers[0].Err.Error(), "belong to another repository") {
			t.Fatalf("answers = %+v, %v", answers, err)
		}
	})

	t.Run("gives up on a repository whose Git never answers, whatever its context says", func(t *testing.T) {
		git := newGitDirs(t)
		root := repositoryAt(t, uuid(1))
		git.hang(root)
		start := time.Now()
		answers, err := gathererOver(git, 100*time.Millisecond).AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{root}}})
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %s", elapsed)
		}
		if err != nil || answers[0].Err == nil || answers[0].Err.Error() != "Git did not answer within 100ms" {
			t.Fatalf("answers = %+v, %v", answers, err)
		}
	})

	t.Run("gives up on a stat that never returns", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		g := worktreestate.New(worktreestate.Options{
			Runner: newGitDirs(t), GitTimeout: 100 * time.Millisecond,
			Stat: func(string) (os.FileInfo, error) { <-release; return nil, errors.New("released") },
		})
		answers, err := g.AskRepositories(ctx, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{filepath.Join(os.TempDir(), "dead-mount")}}})
		if err != nil || answers[0].Err == nil || !strings.Contains(answers[0].Err.Error(), "did not answer within 100ms") {
			t.Fatalf("answers = %+v, %v", answers, err)
		}
	})

	t.Run("holds more than eight repositories to one deadline, and answers them in the order asked however they finish", func(t *testing.T) {
		git := newGitDirs(t)
		const count = 20
		asks := make([]worktreestate.RepositoryAsk, count)
		for i := range asks {
			id := uuid(100 + i)
			asks[i] = worktreestate.RepositoryAsk{ID: id, Roots: []string{repositoryAt(t, id)}}
		}
		// Three never answer; the rest do.
		hung := map[string]bool{uuid(103): true, uuid(111): true, uuid(117): true}
		for _, ask := range asks {
			if hung[ask.ID] {
				git.hang(ask.Roots[0])
			}
		}
		rand.New(rand.NewSource(7)).Shuffle(len(asks), func(i, j int) { asks[i], asks[j] = asks[j], asks[i] })

		start := time.Now()
		answers, err := gathererOver(git, 300*time.Millisecond).AskRepositories(ctx, asks)
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("took %s: the bound is for the whole phase, not for each batch", elapsed)
		}
		if err != nil || len(answers) != count {
			t.Fatalf("answers = %d, %v", len(answers), err)
		}
		for i, answer := range answers {
			if answer.ID != asks[i].ID {
				t.Fatalf("answer %d is %s, want %s: order must follow the asks", i, answer.ID, asks[i].ID)
			}
			if hung[answer.ID] {
				if answer.Err == nil {
					t.Errorf("%s answered", answer.ID)
				}
			} else if answer.Err != nil || len(answer.Entries) != 1 {
				t.Errorf("%s = %+v", answer.ID, answer)
			}
		}
	})

	t.Run("reports every repository it could not reach in time when more than eight hang", func(t *testing.T) {
		git := newGitDirs(t)
		asks := make([]worktreestate.RepositoryAsk, 12)
		for i := range asks {
			id := uuid(100 + i)
			asks[i] = worktreestate.RepositoryAsk{ID: id, Roots: []string{repositoryAt(t, id)}}
			git.hang(asks[i].Roots[0])
		}
		start := time.Now()
		answers, err := gathererOver(git, 200*time.Millisecond).AskRepositories(ctx, asks)
		if elapsed := time.Since(start); elapsed > 3*time.Second || err != nil {
			t.Fatalf("took %s, err %v", elapsed, err)
		}
		for _, answer := range answers {
			if answer.Err == nil {
				t.Errorf("%s answered", answer.ID)
			}
		}
	})

	t.Run("passes on the caller's cancellation", func(t *testing.T) {
		git := newGitDirs(t)
		root := repositoryAt(t, uuid(1))
		git.hang(root)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := gathererOver(git, time.Minute).AskRepositories(cancelled, []worktreestate.RepositoryAsk{{ID: uuid(1), Roots: []string{root}}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWithin(t *testing.T) {
	t.Run("returns what the call returned", func(t *testing.T) {
		got, err := worktreestate.Within(context.Background(), time.Second, func(context.Context) (int, error) { return 7, nil })
		if got != 7 || err != nil {
			t.Fatalf("got %d, %v", got, err)
		}
	})

	t.Run("gives up on a call that ignores its context", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		start := time.Now()
		_, err := worktreestate.Within(context.Background(), 50*time.Millisecond, func(context.Context) (int, error) { <-release; return 1, nil })
		if err == nil || err.Error() != "Git did not answer within 50ms" || time.Since(start) > 5*time.Second {
			t.Fatalf("err = %v after %s", err, time.Since(start))
		}
	})

	t.Run("passes on the caller's cancellation, not as a timeout", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := worktreestate.Within(ctx, time.Minute, func(context.Context) (int, error) { <-release; return 1, nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}
