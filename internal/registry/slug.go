package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/text"
)

// ReservedSlug is the slug no worktree is given: the hostnames of a
// repository's shared services (<name>.shared.<domain>) live beneath it.
const ReservedSlug = layout.SharedHostLabel

// maxSlugLength is a DNS label's limit; slugs stay under it with room for a suffix.
const maxSlugLength = 63

var (
	nonSlugRunes = regexp.MustCompile(`[^a-z0-9]+`)
	edgeHyphens  = regexp.MustCompile(`^-+|-+$`)
	slugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// slugLabelPattern is the stricter shape a slug a person types must
	// have: a DNS label, so it cannot end in a hyphen either. The allocator
	// never produces a trailing hyphen, which is why slugPattern, the shape
	// a stored record is read back against, does not insist on it.
	slugLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// ValidateSlug refuses a slug a person chose that cannot be one: empty, longer
// than a DNS label, not made of lowercase letters, digits and inner hyphens,
// or the reserved slug. Whether the slug is free in a domain is a separate
// question, answered under the registry lock by Store.Rename.
func ValidateSlug(slug string) error {
	switch {
	case slug == "":
		return clierr.New("A slug cannot be empty.")
	case len(slug) > maxSlugLength:
		return clierr.Newf("The slug %q is longer than %d characters, the limit for a hostname label.", slug, maxSlugLength)
	case slug == ReservedSlug:
		return clierr.Newf("The slug %q is reserved: a repository's shared services are routed beneath it.", slug)
	case !slugLabelPattern.MatchString(slug):
		return clierr.Newf("The slug %q is not a valid hostname label: use lowercase letters, digits and hyphens, "+
			"starting and ending with a letter or digit. A slug is the part before the domain, so it has no dots or capitals.", slug)
	}
	return nil
}

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
// under their users. Reclaiming a bare name is an explicit rename
// (`worktree rename`, Store.Rename), not a side effect of allocation.
//
// The suffix derives from the worktree's own identity, so whichever suffix a
// worktree is given, it is the same one every time.
//
// preferred == "" means no preference, since a slug is never empty.
// The slug "shared" is reserved and is never returned, even when preferred.
func AllocateSlug(identity WorktreeIdentity, domain string, others []RegisteredWorktree, preferred string) (string, error) {
	// The slug "shared" is reserved: <name>.shared.<domain> is where a
	// repository's shared services are routed, so no worktree's own
	// hostname may be shared.<domain> or sit beneath it.
	taken := map[string]bool{ReservedSlug: true}
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
