// Package registrytest is an in-memory model of the handful of git queries
// the registry makes, wired as a fakerun.Handler so it plugs straight into
// fakerun.Runner.
package registrytest

import (
	"context"
	"fmt"
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
}

// NewGitRunner is an empty in-memory model.
func NewGitRunner() *GitRunner {
	return &GitRunner{identities: map[string]identity{}}
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
			return run.Result{}, fmt.Errorf("unknown Git worktree: %s", root)
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
// `git worktree list --porcelain -z` produces, enough for callers that only
// parse root and branch out of it.
func (g *GitRunner) porcelainList() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	for _, root := range g.active {
		branch := g.identities[root].branch
		fmt.Fprintf(&b, "worktree %s\x00HEAD deadbeef\x00branch refs/heads/%s\x00\x00", root, branch)
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
