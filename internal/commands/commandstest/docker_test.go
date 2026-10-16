package commandstest

import (
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/worktree"
)

// TestUpLabelsLikeUp pins that the fake daemon stamps what a real `up` does:
// the format label on containers, and on no named volume
// (docs/design/state.md), so a test never proves ownership of a volume by a
// label the real one does not carry.
func TestUpLabelsLikeUp(t *testing.T) {
	d := NewDocker()
	ref := worktree.Ref{RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676", WorktreeID: "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"}
	ids := d.Up(ref, brand.LabelNamespace, "api")
	format := brand.FormatLabel(brand.LabelNamespace)
	if got := d.Labels("container", ids[0])[format]; got != "1" {
		t.Errorf("container format label = %q", got)
	}
	volume := d.Labels("volume", ids[1])
	if _, stamped := volume[format]; stamped || volume[brand.WorktreeLabel(brand.LabelNamespace)] != ref.WorktreeID {
		t.Errorf("volume labels = %v", volume)
	}
}
