package commands

import "context"

// RegisteredWorktreeCandidate is one registered worktree, as shell
// completion (internal/cli) offers it: named the way the command will read
// it back, with its domain and its last-known directory so two worktrees
// offered together are told apart.
type RegisteredWorktreeCandidate struct {
	WorktreeID string
	// Name is what completion inserts: the worktree's slug when the slug
	// names it alone, else its host, else its UUID (registry.NameCandidate).
	Name   string
	Slug   string
	Domain string
	// Root is "" when the worktree was never brought up (no index card yet)
	// or its card could not be read.
	Root string
}

// RegisteredWorktreeCandidates is every worktree registered anywhere on
// this machine, each under the name registry.Registry.Resolve reads back as
// that worktree alone, for `worktree remove` and `worktree unlock` shell
// completion. domain ("" for all) leaves out the worktrees of other domains,
// as `--domain` narrows a slug.
//
// Deliberately narrower than ListRows: it never asks Git for a worktree's
// live root, only each one's own index card (a single file read). Shell
// completion runs on every keystroke, so it stays one filesystem read per
// candidate, never a subprocess, apart from the one question the resolver
// asks Git when a slug is shared by several repositories. A worktree with no
// card yet completes with an empty Root.
//
// Read-only: it neither locks nor writes, and answers only the readable
// records. A missing or empty registry answers an empty slice with no error.
func RegisteredWorktreeCandidates(ctx context.Context, cctx *CommandContext, domain string) ([]RegisteredWorktreeCandidate, error) {
	named, err := cctx.Registry.NameCandidates(ctx, cctx.nameOptions(domain, nil, false))
	if err != nil {
		return nil, err
	}
	candidates := make([]RegisteredWorktreeCandidate, len(named))
	for i, n := range named {
		candidates[i] = RegisteredWorktreeCandidate{
			WorktreeID: n.Record.WorktreeID, Name: n.Name, Slug: n.Record.Slug, Domain: n.Record.Domain, Root: n.Root,
		}
	}
	return candidates, nil
}
