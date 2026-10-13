package commands

import "testing"

// The marker is a name on disk that `restore`'s refusal and docs/hooks.md
// tell users to look for.
func TestIncompleteMarkerIsPinned(t *testing.T) {
	if incompleteMarker != ".incomplete" {
		t.Fatalf("incompleteMarker = %q, want .incomplete", incompleteMarker)
	}
}
