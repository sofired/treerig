package lock_test

import (
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/lock"
)

func mapGetenv(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestRouterScope(t *testing.T) {
	t.Run("targets the router's own state directory, not a control home", func(t *testing.T) {
		getenv := mapGetenv(map[string]string{"HOME": "/home/dev"})

		scope, err := lock.RouterScope(getenv, "berth-router")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if scope.Description != "shared router" {
			t.Errorf("Description = %q, want %q", scope.Description, "shared router")
		}
		if scope.Markers.Project != "berth-router" {
			t.Errorf("Markers.Project = %q, want %q", scope.Markers.Project, "berth-router")
		}
		if scope.Markers.WorktreeID != "" {
			t.Errorf("Markers.WorktreeID = %q, want empty: the router is not a worktree", scope.Markers.WorktreeID)
		}
		if filepath.Base(scope.Target) != "lock" {
			t.Errorf("Target = %q, want a file named lock", scope.Target)
		}
	})
}
