package cli_test

// Wiring checks for the verbs this package hands to commands.NotPorted, or
// to a commands.* function that is still a SPIKE stub returning it (see
// internal/commands' own "SPIKE: skeleton" comments) — not to the oracle,
// since node runs the real thing for every one of these and would not
// agree with a stub. What is checked instead is that cli.Main reaches the
// right verb string, in the right shape (exit 2, the exact message
// commands.NotPorted builds), which is what would tell us if this
// package's own dispatch — not the stub — were wrong.
//
// up, index, doctor and router up/status/down are wired to
// commands.NotPorted for this spike. The ported verbs (config, worktree
// list/remove, urls, status, down) are covered by their own packages' tests
// and by the parity scripts, not here.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/commands"
)

func TestNotPortedVerbs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		verb string
	}{
		{"up", []string{"up"}, "up"},
		{"index", []string{"index"}, "index"},
		{"doctor", []string{"doctor"}, "doctor"},
		{"router-up", []string{"router", "up"}, "router up"},
		{"router-status", []string{"router", "status"}, "router status"},
		{"router-down", []string{"router", "down"}, "router down"},
		{"worktree-unlock", []string{"worktree", "unlock", "feature-login", "--holder-unverifiable"}, "worktree unlock"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := commands.NotPorted("berth", tc.verb)

			var stdout, stderr bytes.Buffer
			args := append([]string{"berth"}, tc.args...)
			code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, emptyGetenv)

			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			wantErr := "[berth] ERROR: " + want.Error() + "\n"
			if stderr.String() != wantErr {
				t.Errorf("stderr = %q, want %q", stderr.String(), wantErr)
			}
			if code != clierr.Code(want) {
				t.Errorf("exit code = %d, want %d", code, clierr.Code(want))
			}
		})
	}
}

// TestDevNotImplemented checks the one verb whose "not yet" message is
// fixed directly in this package (see program.go) rather than built by
// commands.NotPorted, since it is worded and exit-coded differently
// ('%s dev' is not implemented yet...", not "...is not yet ported...").
// Its wording is also covered end to end, against the oracle, by
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
