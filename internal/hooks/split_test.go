package hooks

import (
	"slices"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	t.Run("splits on whitespace and keeps double-quoted segments whole", func(t *testing.T) {
		got := splitCommand(`./run.sh --name "two words" last`)
		want := []string{"./run.sh", "--name", "two words", "last"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if got := splitCommand(""); len(got) != 0 {
			t.Fatalf("expected no parts for an empty command, got %v", got)
		}
	})
}
