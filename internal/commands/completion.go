package commands

import (
	"context"
	"sort"

	"github.com/sofired/treerig/internal/checkoutindex/card"
)

// RegisteredWorktreeCandidate is one registered worktree, as shell
// completion (internal/cli) offers it: named by its own slug, with its
// domain and its last-known path so two same-named slugs under different
// domains, or two worktrees with the same slug offered together, are told
// apart.
type RegisteredWorktreeCandidate struct {
	WorktreeID string
	Slug       string
	Domain     string
	// Root is "" when the worktree was never brought up (no checkout-index
	// card yet) or its card could not be read.
	Root string
}

// RegisteredWorktreeCandidates is every worktree registered anywhere on
// this machine — the same search space registry.Registry.FindGlobal draws
// an identifier from, not just the repository the command runs in — for
// `worktree remove` and `worktree unlock` shell completion.
//
// Deliberately narrower than ListRows: it never asks Git for a worktree's
// live root, only each one's own checkout-index card (card.ReadIndexCard,
// a single file read). Shell completion runs on every keystroke, so it
// must stay one filesystem read per candidate, never a subprocess — the
// same reasoning `worktree list`'s own live-Git enrichment does not apply
// here, even though the row it completes from is otherwise the same one
// `worktree list --all` prints. A worktree with no card yet completes with
// an empty Root; its row is `list`'s to enrich, not this one's.
//
// Read-only: Registry.ListAll neither locks nor writes, and warns rather
// than fails on a record it cannot read; this answers only the readable
// ones, in the same spirit. A missing or empty registry answers an empty,
// non-nil slice with no error.
func RegisteredWorktreeCandidates(ctx context.Context, cctx *CommandContext) ([]RegisteredWorktreeCandidate, error) {
	records, err := cctx.Registry.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]RegisteredWorktreeCandidate, len(records))
	for i, record := range records {
		root := ""
		if seen, err := card.ReadIndexCard(cctx.ControlHome, record.RepositoryID, record.WorktreeID); err == nil && seen != nil {
			root = seen.Root
		}
		candidates[i] = RegisteredWorktreeCandidate{
			WorktreeID: record.WorktreeID,
			Slug:       record.Slug,
			Domain:     record.Domain,
			Root:       root,
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Slug < candidates[j].Slug })
	return candidates, nil
}
