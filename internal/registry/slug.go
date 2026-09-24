package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/text"
)

// maxSlugLength is a DNS label's limit; slugs stay under it with room for a suffix.
const maxSlugLength = 63

var (
	nonSlugRunes = regexp.MustCompile(`[^a-z0-9]+`)
	edgeHyphens  = regexp.MustCompile(`^-+|-+$`)
	slugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// BranchSlug is a DNS-safe label derived from the branch, or the directory
// name when HEAD is detached. Truncation happens before trimming so the
// result can never end in a hyphen.
//
// text.JSLower, not strings.ToLower: after lowering, every non a-z0-9
// character becomes a hyphen, so what JavaScript's toLowerCase does with a
// character like İ (U+0130) — which Go's own ToLower collapses to a plain
// "i" — changes which characters survive.
func BranchSlug(branch, root string) string {
	source := branch
	if source == "" {
		source = filepath.Base(root)
	}
	dashed := nonSlugRunes.ReplaceAllString(text.JSLower(source), "-")
	if len(dashed) > 48 {
		dashed = dashed[:48]
	}
	trimmed := edgeHyphens.ReplaceAllString(dashed, "")
	if trimmed == "" {
		return "worktree"
	}
	return trimmed
}

// AllocateSlug chooses a slug that is free within domain among others, which
// must not include the worktree being allocated. Preference order:
//
//  1. preferred — the slug this worktree already holds;
//  2. the branch slug;
//  3. the branch slug plus a hash suffix that grows until it is free.
//
// Slugs are sticky. A worktree keeps the slug it holds — suffix included —
// whenever that slug is free in the target domain, and a suffix is never
// healed automatically even when the bare name is free again. "The bare name
// is free" is exactly the transient state a domain rename passes through,
// and healing at that moment is how two clones end up swapping hostnames
// under their users. Reclaiming a bare name would be an explicit rename — a
// verb this tool does not have yet — not a side effect of allocation.
//
// The suffix derives from the worktree's own identity, so whichever suffix a
// worktree is given, it is the same one every time.
//
// preferred == "" means no preference, since a slug is never empty.
func AllocateSlug(identity WorktreeIdentity, domain string, others []RegisteredWorktree, preferred string) (string, error) {
	taken := map[string]bool{}
	for _, record := range others {
		if record.Domain == domain {
			taken[record.Slug] = true
		}
	}
	if preferred != "" && !taken[preferred] {
		return preferred, nil
	}

	base := BranchSlug(identity.Branch, identity.Root)
	if !taken[base] {
		return base, nil
	}

	sum := sha256.Sum256([]byte(identity.RepositoryID + ":" + identity.WorktreeID))
	digest := hex.EncodeToString(sum[:])
	// Longest suffix is 60 hex chars: 2 chars of base + '-' + 60 = 63.
	for length := 12; length <= maxSlugLength-3; length += 4 {
		suffix := digest[:length]
		cut := maxSlugLength - 1 - length
		if cut > len(base) {
			cut = len(base)
		}
		candidate := base[:cut] + "-" + suffix
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", clierr.Newf("Could not allocate a unique slug for %s.", identity.Root)
}
