package cli

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/sofired/treerig/internal/commands"
)

func TestRouteNameCompletions(t *testing.T) {
	routes := []commands.HostRouteName{{Name: "api-v1", Port: 8080}, {Name: "web", Port: 5173}, {Name: "web2", Port: 5174}}

	t.Run("offers every name with the port it targets, keeping the order and offering no files", func(t *testing.T) {
		got, directive := routeNameCompletions(routes, "")
		want := []string{
			"api-v1\thost route to 127.0.0.1:8080",
			"web\thost route to 127.0.0.1:5173",
			"web2\thost route to 127.0.0.1:5174",
		}
		if len(got) != len(want) {
			t.Fatalf("got %q", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("completion %d = %q, want %q", i, got[i], want[i])
			}
		}
		if directive != cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveKeepOrder {
			t.Errorf("directive = %v", directive)
		}
	})

	t.Run("filters by the prefix typed", func(t *testing.T) {
		got, _ := routeNameCompletions(routes, "we")
		if len(got) != 2 || got[0] != "web\thost route to 127.0.0.1:5173" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("offers nothing when there are no routes", func(t *testing.T) {
		got, directive := routeNameCompletions(nil, "")
		if len(got) != 0 || directive&cobra.ShellCompDirectiveNoFileComp == 0 {
			t.Fatalf("got %q, %v", got, directive)
		}
	})
}
