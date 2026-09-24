//go:build integration

package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sofired/berth/internal/layout"
)

// The TypeScript build and the Go build, run one after the other on the same
// fixture — same paths, same UUIDs, the same fake docker — must print the
// same thing, exit the same way, and ask docker the same questions. The
// TypeScript build refreshes the checkout index after `down` and `remove`,
// which the Go build does not port yet; those calls are left out of the
// transcript comparison, and succeed silently in the TypeScript run.

const (
	parityRepo = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	parityWt   = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
)

type parityRun struct {
	Stdout, Stderr string
	Exit           int
	Docker         []string
}

type parityFixture struct {
	base, checkout, controlHome, bin, state string
}

// newParityFixture lays out a registered worktree at base, from scratch: a
// git checkout with fixed identity markers, a complete record, a home, and a
// fake docker holding one container and one volume of it.
func newParityFixture(t *testing.T, base string, decoy bool) parityFixture {
	t.Helper()
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	f := parityFixture{base: base, checkout: filepath.Join(base, "checkout"), controlHome: filepath.Join(base, "control"),
		bin: filepath.Join(base, "bin"), state: filepath.Join(base, "state")}
	for _, dir := range []string{f.checkout, f.bin, f.state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.checkout, "berth.toml"), "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n[[service]]\nname = \"api\"\n[[service]]\nname = \"mail\"\nsubdomain = \"mail\"\n", 0o644)
	git := exec.Command("git", "init", "--initial-branch=feature/login", ".")
	git.Dir = f.checkout
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	write(filepath.Join(f.checkout, ".git", "berth", "repository-id"), parityRepo+"\n", 0o600)
	write(filepath.Join(f.checkout, ".git", "berth", "worktree-id"), parityWt+"\n", 0o600)
	paths := layout.WorktreeLayout(f.controlHome, parityRepo, parityWt)
	// Empty, as every other case needs: a non-empty (Go-format) marker
	// would make the TypeScript build itself refuse to touch this control
	// home (assertNotGoManaged), failing every other case here. doctor's
	// own case excludes the one line this leaves it printing that
	// TypeScript never does (files.LegacyControlHome's "was last used by a
	// TypeScript build" notice) the same way it excludes the Node.js line.
	write(filepath.Join(f.controlHome, ".berth-control-home"), "", 0o600)
	write(paths.RecordPath, `{
  "version": 1,
  "repositoryId": "`+parityRepo+`",
  "worktreeId": "`+parityWt+`",
  "slug": "feature-login",
  "domain": "acme.localhost",
  "labelNamespace": "dev.berth",
  "initialization": "complete"
}
`, 0o600)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	project := layout.ComposeProjectName(parityRepo, parityWt)
	owner := parityWt
	if decoy {
		owner = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	}
	// State is files: c1 (a container of api) and v1 (a volume) exist until removed.
	write(filepath.Join(f.bin, "docker"), `#!/bin/sh
state="`+f.state+`"
echo "$*" >> "$state/argv"
case " $* " in
  *" compose "*" config "*) echo '{"name":"x","services":{"api":{"image":"busybox"},"mail":{"image":"busybox"}}}'; exit 0 ;;
  *" compose "*" ps "*) [ -e "$state/c1" ] && echo '{"Service":"api","State":"running","Health":""}'; exit 0 ;;
  *" compose "*" down "*) rm -f "$state/c1"; exit 0 ;;
  *" --filter "*)
    case "$1" in
      ps) case " $* " in *"label=dev.berth.managed=true --format"*) exit 0 ;; esac; [ -e "$state/c1" ] && echo c1 ;;
      volume) [ -e "$state/v1" ] && echo v1 ;;
    esac
    exit 0 ;;
  *" --format "*)
    id=$(eval echo "\${$#}")
    case "$id" in
      c1) printf 'true\t`+project+`\t`+parityRepo+`\t`+parityWt+`\tapi\n' ;;
      v1) printf 'true\t`+project+`\t`+parityRepo+`\t`+owner+`\t\n' ;;
      *) echo "Error: No such object: $id" >&2; exit 1 ;;
    esac
    exit 0 ;;
  *" rm "*) id=$(eval echo "\${$#}"); rm -f "$state/$id"; exit 0 ;;
esac
exit 0
`, 0o755)
	write(filepath.Join(f.state, "c1"), "", 0o600)
	write(filepath.Join(f.state, "v1"), "", 0o600)
	return f
}

// excludeLines drops every line of stdout containing substr — doctor's own
// Node.js and legacy-control-home lines, excluded from the parity
// comparison the way the checkout-index refresh's docker calls are excluded
// from the transcript.
func excludeLines(stdout, substr string) string {
	lines := strings.Split(stdout, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !strings.Contains(line, substr) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// runIn runs command against dir: the fixture's own checkout for every case
// but doctor's "no checkout" one, which needs a directory with no berth.toml
// above it — f.base (this fixture's own root, holding only checkout/, bin/,
// state/ and control/) is that.
func (f parityFixture) runIn(t *testing.T, dir string, command []string) parityRun {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + f.base, "BERTH_HOME=" + f.controlHome,
		"PATH=" + f.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v: %v", command, err)
		}
		exit = exitError.ExitCode()
	}
	transcript, _ := os.ReadFile(filepath.Join(f.state, "argv"))
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(transcript)), "\n") {
		// The TypeScript build's index refresh, which the Go build does not port.
		if line == "" || strings.Contains(line, "label=dev.berth.managed=true --format") {
			continue
		}
		calls = append(calls, line)
	}
	return parityRun{Stdout: stdout.String(), Stderr: stderr.String(), Exit: exit, Docker: calls}
}

func TestParityWithTheTypeScriptBuild(t *testing.T) {
	binary := buildBinary(t)
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 24 is not on PATH")
	}
	ts := []string{nodeBin, filepath.Join(root, "src", "cli", "main.ts")}
	base, _ := filepath.EvalSymlinks(t.TempDir())

	cases := []struct {
		name  string
		args  []string
		decoy bool
		// outsideCheckout runs from the fixture's own base instead of its
		// checkout — doctor's "no checkout" case needs a directory with no
		// berth.toml above it.
		outsideCheckout bool
		// excludeStdoutLines drops any stdout line containing one of these
		// substrings from both builds' report before comparing — doctor's
		// Node.js line, which only TypeScript ever prints
		// (docs/port/divergences.md row 16), and its "was last used by a
		// TypeScript build" line, which only Go ever prints
		// (files.LegacyControlHome; the fixture's own marker must stay
		// empty, or the TypeScript build refuses to touch it at all) — the
		// same way the checkout-index refresh's own docker calls are
		// excluded from the transcript above.
		excludeStdoutLines []string
		// unorderedDocker compares Docker as a set instead of a sequence:
		// doctor asks Docker Engine and Compose concurrently in both
		// builds, and which one's call lands in the transcript first is not
		// itself meaningful.
		unorderedDocker bool
	}{
		{name: "status", args: []string{"status"}},
		{name: "urls", args: []string{"urls"}},
		{name: "worktree list", args: []string{"worktree", "list"}},
		{name: "worktree list --json", args: []string{"worktree", "list", "--json"}},
		{name: "down", args: []string{"down"}},
		{name: "worktree remove --yes", args: []string{"worktree", "remove", "--yes"}},
		{name: "worktree remove without --yes, not a terminal", args: []string{"worktree", "remove"}},
		{name: "worktree remove --yes, refusing a decoy", args: []string{"worktree", "remove", "--yes"}, decoy: true},
		{name: "worktree remove of an unknown slug", args: []string{"worktree", "remove", "--yes", "nope"}},
		{name: "worktree remove --label-namespace that disagrees", args: []string{"worktree", "remove", "--yes", "--label-namespace", "dev.wrong"}},
		{name: "worktree remove --label-namespace that is not one", args: []string{"worktree", "remove", "--yes", "--label-namespace", "Not One"}},
		// worktree unlock: only the refusals that never reach either build's
		// own lock design (an unregistered identifier, or a worktree
		// nothing has locked at all) are byte-identical; everything past
		// that point is Go's own wording (docs/port/divergences.md row 18),
		// so those cases are not here.
		{name: "worktree unlock of an unknown identifier", args: []string{"worktree", "unlock", "nope"}},
		{name: "worktree unlock with nothing held", args: []string{"worktree", "unlock"}},
		// doctor: every other line (Prerequisites' Git/Docker checks,
		// Registry, Locks, Router, This checkout) is byte-identical once the
		// Node.js and legacy-control-home lines are excluded — removing the
		// Node.js line does not even shift the Prerequisites column width,
		// since "Docker Compose" is still the longest subject either way.
		{
			name: "doctor with no checkout", args: []string{"doctor"}, outsideCheckout: true,
			excludeStdoutLines: []string{"Node.js", "was last used by a TypeScript build"},
			unorderedDocker:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := filepath.Join(base, "fixture")
			wantFixture := newParityFixture(t, fixture, tc.decoy)
			wantDir := wantFixture.checkout
			if tc.outsideCheckout {
				wantDir = wantFixture.base
			}
			want := wantFixture.runIn(t, wantDir, append(append([]string{}, ts...), tc.args...))
			gotFixture := newParityFixture(t, fixture, tc.decoy)
			gotDir := gotFixture.checkout
			if tc.outsideCheckout {
				gotDir = gotFixture.base
			}
			got := gotFixture.runIn(t, gotDir, append([]string{binary}, tc.args...))
			for _, substr := range tc.excludeStdoutLines {
				want.Stdout = excludeLines(want.Stdout, substr)
				got.Stdout = excludeLines(got.Stdout, substr)
			}
			if tc.unorderedDocker {
				sort.Strings(want.Docker)
				sort.Strings(got.Docker)
			}
			t.Logf("exit %d, %d docker calls, stdout %q, stderr %q", want.Exit, len(want.Docker), want.Stdout, want.Stderr)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("TypeScript (-) and Go (+) differ:\n%s", diff)
			}
		})
	}
}
