//go:build integration

package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sofired/berth/internal/layout"
)

// isoTimestampPattern matches an ISO-8601 instant to the millisecond, the
// shape both builds' `new Date().toISOString()`/time.Format print for
// IndexSnapshot.GeneratedAt — the one thing that legitimately differs
// between two separate process invocations of otherwise-identical builds,
// since each reads the real clock at the moment it runs. Nothing else in
// either build's output or the page it writes takes this shape (every
// fixture UUID, host and path is fixed), so this is narrow by construction,
// not by exclusion of a known-differing field.
var isoTimestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`)

// normalizeTimestamp replaces every real-time instant in s with a fixed
// placeholder, so two runs that differ only in when they happened compare
// equal. Applied to a run's stdout/stderr and to the index page it wrote,
// never to the docker transcript (which carries no timestamp at all) or to
// the exit code.
func normalizeTimestamp(s string) string {
	return isoTimestampPattern.ReplaceAllString(s, "TIMESTAMP")
}

// The TypeScript build and the Go build, run one after the other on the same
// fixture — same paths, same UUIDs, the same fake docker — must print the
// same thing, exit the same way, ask docker the same questions, and (where a
// case's own comment says so) write the same checkout index page. Both
// builds refresh the index after `down`, `worktree remove`, and `index`
// itself (#41), so every docker call each makes is compared, with no
// exclusion. The one thing that can never match byte for byte between two
// separate process invocations is real wall-clock time: normalizeTimestamp
// scrubs it, narrowly, everywhere a run's own output or the page it wrote
// could carry one.

const (
	parityRepo = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	parityWt   = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
)

type parityRun struct {
	Stdout, Stderr string
	Exit           int
	Docker         []string
	// IndexPage is the checkout index page on disk after the run, timestamp
	// normalized, for a case that asks readIndexPage to read it (down and
	// worktree remove both refresh it as a side effect; "" for a case that
	// does not check it, so cases that never touch the index need no change).
	IndexPage string
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

// run executes command against this fixture and reports what it did.
// checkIndexPage additionally reads the checkout index page HOME's router
// state directory holds afterward (a case whose verb refreshes the index as
// a side effect — down, worktree remove, or index itself); a case that never
// touches the index passes false, so IndexPage stays "" for both builds and
// contributes nothing to the comparison.
func (f parityFixture) run(t *testing.T, command []string, checkIndexPage bool) parityRun {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = f.checkout
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
		if line == "" {
			continue
		}
		calls = append(calls, line)
	}
	run := parityRun{
		Stdout: normalizeTimestamp(stdout.String()), Stderr: normalizeTimestamp(stderr.String()),
		Exit: exit, Docker: calls,
	}
	if checkIndexPage {
		run.IndexPage = normalizeTimestamp(string(f.readIndexPage(t)))
	}
	return run
}

// readIndexPage reads whichever build's own index page this fixture's last
// run wrote. The two builds keep it at different paths — row 9 in the
// divergence ledger: the TypeScript build still writes it under its control
// home (controlLayout(controlHome).indexFile, src/layout.ts), while the Go
// build writes it to the machine-wide router state layout.RouterState
// resolves from HOME alone — so this checks both and returns whichever one
// exists. Each call to run() rebuilds the fixture from scratch first (see
// newParityFixture), so only the build that actually ran can have left a
// file at either path; "" if neither did.
func (f parityFixture) readIndexPage(t *testing.T) []byte {
	t.Helper()
	tsPath := filepath.Join(f.controlHome, "router", "index", "index.html")
	if page, err := os.ReadFile(tsPath); err == nil {
		return page
	} else if !os.IsNotExist(err) {
		t.Fatalf("reading the TypeScript build's index page: %v", err)
	}
	state, err := layout.RouterState(func(key string) (string, bool) {
		if key == "HOME" {
			return f.base, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("resolving the Go build's router state: %v", err)
	}
	page, err := os.ReadFile(state.IndexFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the Go build's index page: %v", err)
	}
	return page
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
		// checkIndexPage additionally compares the checkout index page on
		// disk (timestamp normalized) for a case whose verb refreshes it.
		checkIndexPage bool
	}{
		{"status", []string{"status"}, false, false},
		{"urls", []string{"urls"}, false, false},
		// index's own stdout embeds snapshot.GeneratedAt, real wall-clock
		// time at the moment each build's own process ran; normalizeTimestamp
		// scrubs it (both here and in the page below) rather than excluding
		// the case, so the rest of index's output and every docker call it
		// makes are still compared exactly.
		{"index", []string{"index"}, false, true},
		{"worktree list", []string{"worktree", "list"}, false, false},
		{"worktree list --json", []string{"worktree", "list", "--json"}, false, false},
		{"down", []string{"down"}, false, true},
		{"worktree remove --yes", []string{"worktree", "remove", "--yes"}, false, true},
		{"worktree remove without --yes, not a terminal", []string{"worktree", "remove"}, false, false},
		{"worktree remove --yes, refusing a decoy", []string{"worktree", "remove", "--yes"}, true, false},
		{"worktree remove of an unknown slug", []string{"worktree", "remove", "--yes", "nope"}, false, false},
		{"worktree remove --label-namespace that disagrees", []string{"worktree", "remove", "--yes", "--label-namespace", "dev.wrong"}, false, false},
		{"worktree remove --label-namespace that is not one", []string{"worktree", "remove", "--yes", "--label-namespace", "Not One"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := filepath.Join(base, "fixture")
			want := newParityFixture(t, fixture, tc.decoy).run(t, append(append([]string{}, ts...), tc.args...), tc.checkIndexPage)
			got := newParityFixture(t, fixture, tc.decoy).run(t, append([]string{binary}, tc.args...), tc.checkIndexPage)
			t.Logf("exit %d, %d docker calls, stdout %q, stderr %q", want.Exit, len(want.Docker), want.Stdout, want.Stderr)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("TypeScript (-) and Go (+) differ:\n%s", diff)
			}
		})
	}
}
