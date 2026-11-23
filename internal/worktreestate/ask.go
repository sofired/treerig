package worktreestate

import (
	"context"
	"path/filepath"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/registry"
)

// DefaultGitTimeout bounds everything asked of Git in one run: all the other
// repositories together, and the current one. Git on a hung mount can wait
// forever in a system call that no signal reaches, so the bound is on how long
// the caller waits, not on the work.
const DefaultGitTimeout = 5 * time.Second

// askConcurrency bounds how many repositories are asked at once.
const askConcurrency = 8

// RepositoryAsk names a repository and the directories its worktrees were last
// seen in (their index cards' roots), the only paths into it the registry has.
type RepositoryAsk struct {
	ID    string
	Roots []string
}

// RepositoryAnswer is what AskRepositories found out about one repository.
type RepositoryAnswer struct {
	ID string
	// Asked is false for a repository with no recorded directory at all: there
	// is no path into it, so Git was not asked and its worktrees have no
	// directory evidence, as they never did.
	Asked bool
	// Entries is what Git lists. It is nil with a nil Err when none of the
	// recorded directories is there: the worktrees are then judged by those
	// directories alone, which is all that can be said of them.
	Entries []registry.WorktreeEntry
	// Err is why Git could not say: it did not answer in time, failed, or the
	// directories it was asked in belong to another repository. The
	// repository's worktrees are then unknown.
	Err error
}

// errDidNotAnswer is the cause of a repository that was not heard from in time.
func errDidNotAnswer(budget time.Duration) error {
	return clierr.Newf("Git did not answer within %s", budget)
}

// Within runs fn in a goroutine of its own and waits for it for at most budget.
// A caller of Git or the file system on a hung mount would otherwise wait as
// long as the mount does, whatever its context says, since a process stuck in
// the kernel ignores the signal that cancels it. A call that is abandoned is
// answered with "Git did not answer within the budget"; it finishes, or stays
// stuck, on its own, and its result is dropped (the channel is buffered, so it
// never blocks). The caller's own cancellation is returned as ctx.Err().
func Within[T any](ctx context.Context, budget time.Duration, fn func(context.Context) (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		value, err := fn(deadline)
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-deadline.Done():
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return zero, errDidNotAnswer(budget)
	}
}

// AskRepositories asks Git about every repository in asks and returns the
// answers in the order of asks, whatever order they came in. The work of each
// repository (its directories' stat, Git, the identities of its worktrees) runs
// in a goroutine, at most eight at a time, and the whole phase has one deadline,
// Options.GitTimeout: a repository still working at it, or never started, is
// answered "Git did not answer within …" and its goroutine abandoned. Only the
// caller's cancellation is an error.
//
// Git is run in the first recorded directory that is an absolute path to a
// directory and whose listing has an entry of this repository's identity; a
// directory that has come to belong to another repository is passed over for
// the next, and when none belongs the repository's Err says so.
func (g *Gatherer) AskRepositories(ctx context.Context, asks []RepositoryAsk) ([]RepositoryAnswer, error) {
	answers := make([]RepositoryAnswer, len(asks))
	for i, ask := range asks {
		answers[i] = RepositoryAnswer{ID: ask.ID, Asked: len(ask.Roots) > 0}
	}
	deadline, cancel := context.WithTimeout(ctx, g.gitTimeout)
	defer cancel()

	type finished struct {
		index   int
		entries []registry.WorktreeEntry
		err     error
	}
	// Buffered for every repository: an abandoned worker's late answer is
	// dropped without blocking it, so it can end.
	results := make(chan finished, len(asks))
	slots := make(chan struct{}, askConcurrency)
	started := 0
dispatch:
	for i, ask := range asks {
		if !answers[i].Asked {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-deadline.Done():
			break dispatch
		}
		started++
		go func() {
			entries, err := g.askRepository(deadline, ask)
			<-slots
			results <- finished{i, entries, err}
		}()
	}

	heard := make([]bool, len(asks))
collect:
	for received := 0; received < started; received++ {
		select {
		case r := <-results:
			answers[r.index].Entries, answers[r.index].Err = r.entries, r.err
			heard[r.index] = true
		case <-deadline.Done():
			break collect
		}
	}
	// An answer that arrived as the deadline did is still an answer.
drain:
	for {
		select {
		case r := <-results:
			if !heard[r.index] {
				answers[r.index].Entries, answers[r.index].Err = r.entries, r.err
				heard[r.index] = true
			}
		default:
			break drain
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i := range answers {
		if answers[i].Asked && !heard[i] {
			answers[i].Err = errDidNotAnswer(g.gitTimeout)
		}
	}
	return answers, nil
}

// askRepository lists the worktrees of one repository through the first of its
// recorded directories that works, as AskRepositories says.
func (g *Gatherer) askRepository(ctx context.Context, ask RepositoryAsk) ([]registry.WorktreeEntry, error) {
	var last error
	for _, root := range ask.Roots {
		if !filepath.IsAbs(root) {
			continue
		}
		if info, err := g.stat(root); err != nil || !info.IsDir() {
			continue
		}
		entries, err := registry.NewIdentityService(root, g.runner, g.environ).Entries(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			continue
		}
		if belongsTo(entries, ask.ID) {
			return entries, nil
		}
		last = clierr.New("The directories it was last seen in now belong to another repository")
	}
	return nil, last
}

// belongsTo reports whether a listing has a worktree of repository id: the
// listing is of that repository, not of whatever now lives where it used to be.
func belongsTo(entries []registry.WorktreeEntry, id string) bool {
	for _, entry := range entries {
		if entry.RepositoryID == id {
			return true
		}
	}
	return false
}
