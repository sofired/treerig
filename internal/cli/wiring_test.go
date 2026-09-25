package cli_test

// Wiring checks for the one verb whose "not yet" answer is fixed in this
// package rather than in a command body.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
)

// TestDevNotImplemented checks `dev`, which neither build implements yet:
// its message is fixed directly in this package (see program.go). Its wording is also covered end to end, against the oracle, by
// TestGolden's dev-not-implemented case; this one exists to pin the exit
// code and stream by name.
func TestDevNotImplemented(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cli.Main([]string{"berth", "dev"}, strings.NewReader(""), &stdout, &stderr, emptyGetenv)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	const want = "[berth] ERROR: 'berth dev' is not implemented yet; see the Roadmap in README.md.\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
