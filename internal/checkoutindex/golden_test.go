package checkoutindex

import (
	"encoding/json"
	"os"
	"testing"
)

// TestIndexGolden checks RenderIndexHTML and FormatIndexText against
// internal/checkoutindex/testdata/index-golden.json, a hand-maintained
// fixture covering: empty, placeholder, several domains and projects,
// missing and unknown worktrees, a checkout never brought up, hostile
// names, long paths needing wrap, and non-ASCII. Every byte must match.
//
// The fixtures decode straight into IndexSnapshot/IndexEntry: encoding/json
// matches a camelCase JSON key against an exported Go field case-
// insensitively when no tag says otherwise, so "repositoryId" reaches
// RepositoryID, "branch" reaches Branch (nil when the key is absent, a
// pointer to "" when it is present and empty — distinguishing "absent"
// from "empty"), and so on with no translation layer needed.
func TestIndexGolden(t *testing.T) {
	// Every fixture root is under /work/…, but pin it anyway: RenderIndexHTML
	// and FormatIndexText compact each root against the real home directory,
	// and this test should not depend on what that happens to be wherever it
	// runs (a runner with HOME=/ would otherwise compact every root to
	// "~/work/…" and fail every fixture that carries one).
	pinHomeDir(t, "/home/treerig-test")

	data, err := os.ReadFile("testdata/index-golden.json")
	if err != nil {
		t.Fatalf("reading golden data: %v", err)
	}
	var golden struct {
		URL      string `json:"url"`
		Fixtures []struct {
			Name     string        `json:"name"`
			Snapshot IndexSnapshot `json:"snapshot"`
			HTML     string        `json:"html"`
			Text     string        `json:"text"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("parsing golden data: %v", err)
	}
	if len(golden.Fixtures) == 0 {
		t.Fatal("no golden fixtures in hand-maintained testdata/index-golden.json")
	}

	for _, fixture := range golden.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if got := RenderIndexHTML(fixture.Snapshot); got != fixture.HTML {
				t.Errorf("RenderIndexHTML mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, fixture.HTML)
			}
			if got := FormatIndexText(fixture.Snapshot, golden.URL); got != fixture.Text {
				t.Errorf("FormatIndexText mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, fixture.Text)
			}
		})
	}
}
