package brand_test

// Tests for the parts that apply to CLIName.
//
// Not covered here:
//   - "names an example config that is actually in the tree": a filesystem
//     check on BRAND.exampleFile, nothing to do with CLIName.
//   - "strips a script extension from a launcher name": CLIName takes
//     argv[0] directly, and a Go binary never carries a launcher script
//     extension for it to strip.

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
		// An empty argv[0] leaves no meaningful name. A binary built
		// without -o and run as `./main` carries the same "no meaningful
		// name" case — see brand.go's own doc comment.
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
