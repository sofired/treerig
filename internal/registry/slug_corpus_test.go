package registry_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sofired/berth/internal/registry"
)

// TestBranchSlugMatchesJavaScript checks BranchSlug against
// testdata/slug-corpus.json, a hand-maintained fixture holding the
// expected branchSlug output for the same inputs (its origin is in git
// history). The corpus exists for İ (U+0130): JavaScript's toLowerCase
// gives "i" plus a combining dot above, which Go's strings.ToLower alone
// would not reproduce — see internal/text.JSLower, which BranchSlug uses
// instead.
func TestBranchSlugMatchesJavaScript(t *testing.T) {
	data, err := os.ReadFile("testdata/slug-corpus.json")
	if err != nil {
		t.Fatalf("reading fixture data: %v", err)
	}
	var cases []struct {
		Name   string `json:"name"`
		Branch string `json:"branch"`
		Root   string `json:"root"`
		Slug   string `json:"slug"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing fixture data: %v", err)
	}
	if len(cases) == 0 {
		t.Fatalf("empty fixture corpus")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if got := registry.BranchSlug(c.Branch, c.Root); got != c.Slug {
				t.Fatalf("BranchSlug(%q, %q) = %q, want %q", c.Branch, c.Root, got, c.Slug)
			}
		})
	}
}
