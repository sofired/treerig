package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/sofired/berth/internal/clierr"
)

// BERTH_DEBUG prints the chain of causes, one "type: message" line each,
// since Go has no stack trace the way TypeScript's build once printed one.
func TestErrorChain(t *testing.T) {
	t.Run("adds nothing for one of berth's own errors with no cause", func(t *testing.T) {
		if got := errorChain(clierr.New("No berth.toml here.")); got != "" {
			t.Fatalf("got %q, want nothing", got)
		}
	})
	t.Run("lists each link of the chain with its type", func(t *testing.T) {
		err := clierr.Wrap("Could not read the registry.", fmt.Errorf("open /x: %w", fs.ErrPermission))
		want := "*clierr.Error: Could not read the registry.\n" +
			"*fmt.wrapError: open /x: permission denied\n" +
			"*errors.errorString: permission denied"
		if got := errorChain(err); got != want {
			t.Fatalf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("describes an unexpected error even with no cause", func(t *testing.T) {
		if got, want := errorChain(errors.New("boom")), "*errors.errorString: boom"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}
