package registry_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/registry"
)

const (
	acmeDomain = "acme.localhost"
)

func slugIdentity(branch, root string) registry.WorktreeIdentity {
	if root == "" {
		root = "/repo"
	}
	return registry.WorktreeIdentity{
		Root: root, Branch: branch, GitDir: "/g", GitCommonDir: "/g",
		RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676",
		WorktreeID:   "2b41d9d4-ed88-49c1-be9c-5439bdf6a431",
	}
}

func TestSlugDerivation(t *testing.T) {
	t.Run("never ends in a hyphen even when truncation lands on one", func(t *testing.T) {
		aRun47 := strings.Repeat("a", 47) + "-b"
		if got := registry.BranchSlug(aRun47, "/repo"); got != strings.Repeat("a", 47) {
			t.Fatalf("got %q", got)
		}
		nested := "feature/" + strings.Repeat("x", 39) + "/y"
		if got := registry.BranchSlug(nested, "/repo"); !regexp.MustCompile(`[a-z0-9]$`).MatchString(got) {
			t.Fatalf("expected to end in alphanumeric, got %q", got)
		}
		pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)
		for _, slug := range []string{
			registry.BranchSlug(aRun47, "/repo"),
			registry.BranchSlug(strings.Repeat("x", 60), "/repo"),
		} {
			if len(slug) > 48 {
				t.Fatalf("slug %q longer than 48", slug)
			}
			if !pattern.MatchString(slug) {
				t.Fatalf("slug %q does not match expected shape", slug)
			}
		}
	})

	t.Run("falls back to the directory name when detached, and to a constant when nothing survives", func(t *testing.T) {
		if got := registry.BranchSlug("", "/work/my-tree"); got != "my-tree" {
			t.Fatalf("got %q", got)
		}
		if got := registry.BranchSlug("---", "/work/---"); got != "worktree" {
			t.Fatalf("got %q", got)
		}
		if got := registry.BranchSlug("日本語", "/repo"); got != "worktree" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("keeps every suffixed candidate within the DNS label limit, however many collide", func(t *testing.T) {
		id := slugIdentity("feature/shared", "")
		digestSum := sha256.Sum256([]byte(id.RepositoryID + ":" + id.WorktreeID))
		digest := hex.EncodeToString(digestSum[:])
		var taken []registry.RegisteredWorktree
		taken = append(taken, registry.RegisteredWorktree{RepositoryID: "r", WorktreeID: "w0", Slug: "feature-shared", Domain: acmeDomain})
		for length := 12; length <= 60; length += 4 {
			suffix := digest[:length]
			cut := 62 - length
			taken = append(taken, registry.RegisteredWorktree{
				RepositoryID: "r", WorktreeID: "w", Slug: "feature-shared"[:min(cut, len("feature-shared"))] + "-" + suffix, Domain: acmeDomain,
			})
		}
		if _, err := registry.AllocateSlug(id, acmeDomain, taken, ""); err == nil {
			t.Fatalf("expected allocation to fail when every candidate collides")
		}
		// With one fewer collision, the longest candidate is used and is exactly 63 chars.
		last := taken[len(taken)-1]
		taken = taken[:len(taken)-1]
		slug, err := registry.AllocateSlug(id, acmeDomain, taken, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if slug != last.Slug {
			t.Fatalf("got %q, want %q", slug, last.Slug)
		}
		if len(slug) > 63 {
			t.Fatalf("slug %q longer than 63", slug)
		}
	})

	t.Run("prefers the slug a worktree already holds over re-deriving it from the branch", func(t *testing.T) {
		id := slugIdentity("feature/renamed-since", "")
		got, err := registry.AllocateSlug(id, acmeDomain, nil, "feature-original")
		if err != nil || got != "feature-original" {
			t.Fatalf("got %q, %v", got, err)
		}
		taken := []registry.RegisteredWorktree{{RepositoryID: "r", WorktreeID: "other", Slug: "feature-original", Domain: acmeDomain}}
		got, err = registry.AllocateSlug(id, acmeDomain, taken, "feature-original")
		if err != nil || got != "feature-renamed-since" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("keeps a suffix it already holds even when the bare name is free, so two clones can never swap", func(t *testing.T) {
		id := slugIdentity("feature/login", "")
		got, err := registry.AllocateSlug(id, acmeDomain, nil, "feature-login-1d407e139915")
		if err != nil || got != "feature-login-1d407e139915" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}
