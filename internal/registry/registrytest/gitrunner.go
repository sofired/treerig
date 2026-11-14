// Package registrytest is an in-memory model of the handful of git queries
// the registry makes, wired as a fakerun.Handler so it plugs straight into
// fakerun.Runner.
package registrytest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

type identity struct {
	root, branch, gitCommonDir, gitDir string
}

// GitRunner answers the handful of git queries the registry makes, from an
// in-memory model.
type GitRunner struct {
	mu         sync.Mutex
	identities map[string]identity
	active     []string
	prunable   map[string]string
	locked     map[string]bool
}

// NewGitRunner is an empty in-memory model.
func NewGitRunner() *GitRunner {
	return &GitRunner{identities: map[string]identity{}, prunable: map[string]string{}, locked: map[string]bool{}}
}

// Add registers a Git worktree at root on branch, sharing common as its
// common Git directory. gitName defaults to root's base name, matching how
// `git worktree add` names the administrative directory.
func (g *GitRunner) Add(root, branch, common string) {
	g.AddNamed(root, branch, common, filepath.Base(root))
}

// AddNamed is Add with an explicit administrative directory name, for a
// worktree whose name was not derived from its path (Git reusing a name, say).
func (g *GitRunner) AddNamed(root, branch, common, gitName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.identities[root] = identity{
		root:         root,
		branch:       branch,
		gitCommonDir: common,
		gitDir:       filepath.Join(common, "worktrees", gitName),
	}
	g.active = append(g.active, root)
}

// SetBranch renames the branch a registered root is on.
func (g *GitRunner) SetBranch(root, branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if current, ok := g.identities[root]; ok {
		current.branch = branch
		g.identities[root] = current
	}
}

// Identity reads back what Add or SetBranch recorded for root.
func (g *GitRunner) Identity(root string) (gitCommonDir, gitDir string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	item, ok := g.identities[root]
	return item.gitCommonDir, item.gitDir, ok
}

// SetIdentity overwrites the identity recorded for root outright, for a test
// that moves a worktree without going through Add (the root changes, but
// the marker directories must not).
func (g *GitRunner) SetIdentity(root, branch, gitCommonDir, gitDir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.identities[root] = identity{root: root, branch: branch, gitCommonDir: gitCommonDir, gitDir: gitDir}
}

// Forget removes root from the identity model without touching Active.
func (g *GitRunner) Forget(root string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.identities, root)
}

// Active is every root `git worktree list` reports, in the order Add saw them.
func (g *GitRunner) Active() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.active...)
}

// SetActive replaces the set `git worktree list` reports, for a test that
// simulates `git worktree remove` without deleting the identity model.
func (g *GitRunner) SetActive(active []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = append([]string(nil), active...)
}

// SetPrunable makes `git worktree list` call root prunable for reason, as Git
// does for an entry whose administrative files are damaged. An entry whose
// directory is not on disk is prunable without this, as in Git.
func (g *GitRunner) SetPrunable(root, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prunable[root] = reason
}

// SetLocked makes `git worktree list` report root locked (or not), as `git
// worktree lock` does.
func (g *GitRunner) SetLocked(root string, locked bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.locked[root] = locked
}

// Handler answers `git` calls from the model; every other command exits 0
// with no output, exactly like fakerun.Runner's own default.
func (g *GitRunner) Handler() fakerun.Handler {
	return func(_ context.Context, spec run.Spec) (run.Result, error) {
		if spec.Command != "git" {
			return run.Result{}, nil
		}
		args := spec.Args
		if contains(args, "worktree") && contains(args, "list") {
			return run.Result{Stdout: g.porcelainList()}, nil
		}
		root := argAfter(args, "-C")
		g.mu.Lock()
		item, ok := g.identities[root]
		g.mu.Unlock()
		if !ok {
			// What Git says outside every repository; run.Finish turns it
			// into the failure a caller without AllowFailure sees.
			return run.Result{ExitCode: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git\n"}, nil
		}
		stdout := item.branch + "\n"
		switch {
		case contains(args, "--show-toplevel"):
			stdout = item.root + "\n"
		case contains(args, "--git-common-dir"):
			stdout = item.gitCommonDir + "\n"
		case contains(args, "--git-dir"):
			stdout = item.gitDir + "\n"
		}
		return run.Result{Stdout: stdout}, nil
	}
}

// porcelainList renders every active worktree in the NUL-separated format
// `git worktree list --porcelain -z` produces: root, branch, and as Git does
// "locked" for a locked entry and "prunable <reason>" for one whose directory
// is not on disk (unless locked) or that SetPrunable marked.
func (g *GitRunner) porcelainList() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	for _, root := range g.active {
		branch := g.identities[root].branch
		fmt.Fprintf(&b, "worktree %s\x00HEAD deadbeef\x00branch refs/heads/%s\x00", root, branch)
		if g.locked[root] {
			b.WriteString("locked\x00")
		}
		reason, prunable := g.prunable[root]
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) && !g.locked[root] {
			reason, prunable = "gitdir file points to non-existent location", true
		}
		if prunable {
			fmt.Fprintf(&b, "prunable %s\x00", reason)
		}
		b.WriteString("\x00")
	}
	return b.String()
}

// contains reports whether target is present in values.
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// argAfter returns the value immediately following flag in values, or "" if
// flag is absent or is the last element.
func argAfter(values []string, flag string) string {
	for i, v := range values {
		if v == flag && i+1 < len(values) {
			return values[i+1]
		}
	}
	return ""
}

// NewFakeRunner is a fakerun.Runner wired to a fresh GitRunner, for tests
// that just need a working directory.
func NewFakeRunner() (*fakerun.Runner, *GitRunner) {
	g := NewGitRunner()
	return &fakerun.Runner{Handler: g.Handler()}, g
}
