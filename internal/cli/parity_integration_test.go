//go:build integration

package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// envGetenv adapts a process env slice ("KEY=VALUE" entries, as cmd.Env
// takes them) into a layout.Getenv-shaped closure, last-match-wins for a
// duplicate key — the way a real process's own environment resolves one,
// and the way this file's and up_integration_test.go's own hand-built
// cmd.Env slices are meant to be read back by registrytest's isolation
// guard and layout.RouterState alike.
func envGetenv(env []string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		prefix := key + "="
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], prefix) {
				return strings.TrimPrefix(env[i], prefix), true
			}
		}
		return "", false
	}
}

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
  *" --project-name berth-router "*" ps "*" --quiet "*) echo routerc1; exit 0 ;;
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

// filterDockerCalls drops every entry of calls containing substr, the same
// way excludeLines drops a matching stdout line — used for a Docker call
// only one build's own new behaviour makes (see excludeDockerCalls above).
func filterDockerCalls(calls []string, substr string) []string {
	kept := calls[:0]
	for _, call := range calls {
		if !strings.Contains(call, substr) {
			kept = append(kept, call)
		}
	}
	return kept
}

// runIn executes command in dir — the fixture's own checkout for every case
// but doctor's "no checkout" one, which needs a directory with no berth.toml
// above it; f.base (this fixture's own root, holding only checkout/, bin/,
// state/ and control/) is that — and reports what it did. checkIndexPage
// additionally reads the checkout index page HOME's router state directory
// holds afterward (a case whose verb refreshes the index as a side effect —
// down, worktree remove, or index itself); a case that never touches the
// index passes false, so IndexPage stays "" for both builds and contributes
// nothing to the comparison.
func (f parityFixture) runIn(t *testing.T, dir string, command []string, checkIndexPage bool) parityRun {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + f.base,
		// BERTH_HOME is what the TypeScript build reads as the control home
		// directly. BERTH_CONTROL_HOME is the Go build's own, more direct
		// override (#80); set alongside it, both builds resolve the same
		// control home from the one fixture. Left set, BERTH_HOME also moves
		// the Go build's router under f.controlHome — readIndexPage below
		// accounts for that.
		"BERTH_HOME=" + f.controlHome, "BERTH_CONTROL_HOME=" + f.controlHome,
		"PATH=" + f.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	}
	registrytest.RequireIsolatedRouterState(t, envGetenv(cmd.Env))
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
// run wrote. Before #80 the two builds kept it at different paths — row 9 in
// the divergence ledger: the TypeScript build under its control home
// (controlLayout(controlHome).indexFile, src/layout.ts), the Go build under
// the machine-wide state layout.RouterState resolved from HOME alone, with
// no BERTH_HOME involved. runIn now sets BERTH_HOME=f.controlHome for the
// TypeScript build's sake (it has no BERTH_CONTROL_HOME of its own), which
// also moves the Go build's own router under f.controlHome/router in this
// fixture specifically — the two paths below coincide as a result, in a way
// they never would in a real environment, where nothing sets BERTH_HOME to
// what the Go build treats as a control home. Both are still checked, and
// this stays the one place a real divergence (a future one, say) would need
// two branches again. Each call to run() rebuilds the fixture from scratch
// first (see newParityFixture), so only the build that actually ran can
// have left a file at either path; "" if neither did.
func (f parityFixture) readIndexPage(t *testing.T) []byte {
	t.Helper()
	tsPath := filepath.Join(f.controlHome, "router", "index", "index.html")
	if page, err := os.ReadFile(tsPath); err == nil {
		return page
	} else if !os.IsNotExist(err) {
		t.Fatalf("reading the TypeScript build's index page: %v", err)
	}
	state, err := layout.RouterState(func(key string) (string, bool) {
		switch key {
		case "HOME":
			return f.base, true
		case "BERTH_HOME":
			return f.controlHome, true
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
		// outsideCheckout runs from the fixture's own base instead of its
		// checkout — doctor's "no checkout" case needs a directory with no
		// berth.toml above it.
		outsideCheckout bool
		// excludeStdoutLines drops any stdout line containing one of these
		// substrings from both builds' report before comparing — doctor's
		// Node.js line, which only TypeScript ever prints
		// (docs/port/divergences.md row 17), and its "was last used by a
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
		// checkIndexPage additionally compares the checkout index page on
		// disk (timestamp normalized) for a case whose verb refreshes it.
		checkIndexPage bool
		// excludeDockerCalls drops any Docker transcript entry containing one
		// of these substrings from both builds before comparing — Go's own
		// router-network disconnect, added by issue #55
		// (docs/port/divergences.md row 31): looking up the router's own
		// container (`compose --project-name berth-router ps --all --quiet
		// router`) and detaching it from this worktree's network (`network
		// disconnect`). TypeScript has no per-worktree router network to
		// disconnect and never makes either call, so this only ever removes
		// entries from Go's own transcript — filtering both sides, the same
		// way excludeStdoutLines does, keeps the two lists' treatment
		// symmetric rather than favouring one build's transcript over the
		// other's.
		excludeDockerCalls []string
	}{
		{name: "status", args: []string{"status"}},
		{name: "urls", args: []string{"urls"}},
		// index's own stdout embeds snapshot.GeneratedAt, real wall-clock
		// time at the moment each build's own process ran; normalizeTimestamp
		// scrubs it (both here and in the page below) rather than excluding
		// the case, so the rest of index's output and every docker call it
		// makes are still compared exactly.
		{name: "index", args: []string{"index"}, checkIndexPage: true},
		{name: "worktree list", args: []string{"worktree", "list"}},
		{name: "worktree list --json", args: []string{"worktree", "list", "--json"}},
		{
			name: "down", args: []string{"down"}, checkIndexPage: true,
			excludeDockerCalls: []string{"--project-name berth-router ps", "network disconnect"},
		},
		{
			name: "worktree remove --yes", args: []string{"worktree", "remove", "--yes"}, checkIndexPage: true,
			excludeDockerCalls: []string{"--project-name berth-router ps", "network disconnect"},
		},
		{name: "worktree remove without --yes, not a terminal", args: []string{"worktree", "remove"}},
		{name: "worktree remove --yes, refusing a decoy", args: []string{"worktree", "remove", "--yes"}, decoy: true},
		{name: "worktree remove of an unknown slug", args: []string{"worktree", "remove", "--yes", "nope"}},
		{name: "worktree remove --label-namespace that disagrees", args: []string{"worktree", "remove", "--yes", "--label-namespace", "dev.wrong"}},
		{name: "worktree remove --label-namespace that is not one", args: []string{"worktree", "remove", "--yes", "--label-namespace", "Not One"}},
		// worktree unlock: only the refusals that never reach either build's
		// own lock design (an unregistered identifier, or a worktree
		// nothing has locked at all) are byte-identical; everything past
		// that point is Go's own wording (docs/port/divergences.md row 19),
		// so those cases are not here.
		{name: "worktree unlock of an unknown identifier", args: []string{"worktree", "unlock", "nope"}},
		{name: "worktree unlock with nothing held", args: []string{"worktree", "unlock"}},
		// doctor: every other line (Prerequisites' Git/Docker checks,
		// Registry, Locks, Router, This checkout) is byte-identical once the
		// Node.js, legacy-control-home, berth-root and router-state lines
		// are excluded — the last two are Go's own, for the berth root and
		// the router's own state directory (#80, docs/port/divergences.md
		// row 29), which TypeScript has no counterpart for — removing the
		// Node.js line does not even shift the Prerequisites column width,
		// since "Docker Compose" is still the longest subject either way.
		{
			name: "doctor with no checkout", args: []string{"doctor"}, outsideCheckout: true,
			excludeStdoutLines: []string{
				"Node.js", "was last used by a TypeScript build",
				"berth root", "router state",
			},
			unorderedDocker: true,
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
			want := wantFixture.runIn(t, wantDir, append(append([]string{}, ts...), tc.args...), tc.checkIndexPage)
			gotFixture := newParityFixture(t, fixture, tc.decoy)
			gotDir := gotFixture.checkout
			if tc.outsideCheckout {
				gotDir = gotFixture.base
			}
			got := gotFixture.runIn(t, gotDir, append([]string{binary}, tc.args...), tc.checkIndexPage)
			for _, substr := range tc.excludeStdoutLines {
				want.Stdout = excludeLines(want.Stdout, substr)
				got.Stdout = excludeLines(got.Stdout, substr)
			}
			for _, substr := range tc.excludeDockerCalls {
				want.Docker = filterDockerCalls(want.Docker, substr)
				got.Docker = filterDockerCalls(got.Docker, substr)
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
