package brand_test

// Ported from tests/unit/brand.test.ts's `describe('cliName', ...)` block —
// the parts that apply to CLIName. Test titles are kept verbatim so coverage
// can be compared title by title with the TypeScript suite.
//
// Not ported:
//   - "names an example config that is actually in the tree": a filesystem
//     check on BRAND.exampleFile, nothing to do with CLIName.
//   - "strips a script extension from a launcher name": CLIName takes
//     argv[0] directly rather than JavaScript's argv[1] (the interpreter is
//     argv[0] there), and a Go binary never carries a .mjs/.ts launcher
//     extension for it to strip. Accepted difference.

import (
	"testing"

	"github.com/sofired/berth/internal/brand"
)

func TestCLIName(t *testing.T) {
	t.Run("follows the installed binary or symlink name", func(t *testing.T) {
		if got := brand.CLIName("/usr/local/bin/duke"); got != "duke" {
			t.Errorf("CLIName(%q) = %q, want %q", "/usr/local/bin/duke", got, "duke")
		}
		if got := brand.CLIName("/opt/tools/wt"); got != "wt" {
			t.Errorf("CLIName(%q) = %q, want %q", "/opt/tools/wt", got, "wt")
		}
	})

	t.Run("falls back to the product name when running the source directly", func(t *testing.T) {
		// TypeScript's argv[1] is empty when running interactively (`node`
		// alone); Go's analogue is an empty argv[0], both leaving no
		// meaningful name. A binary built without -o and run as `./main`
		// carries the same "no meaningful name" case for Go specifically —
		// see brand.go's own doc comment.
		if got := brand.CLIName(""); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "", got, brand.Name)
		}
		if got := brand.CLIName("main"); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "main", got, brand.Name)
		}
		if got := brand.CLIName("./main"); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "./main", got, brand.Name)
		}
	})
}
