//go:build integration

package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
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
// shape treerig prints for IndexSnapshot.GeneratedAt — the one thing that
// legitimately differs between two runs, since each reads the real clock at
// the moment it runs. Nothing else in the output or the page it writes takes
// this shape (every fixture UUID and host is fixed), so this is narrow by
// construction, not by exclusion of a known-differing field.
var isoTimestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`)

// normalizeTimestamp replaces every real-time instant in s with a fixed
// placeholder, so two runs that differ only in when they happened compare
// equal. Applied to a run's stdout/stderr and to the index page it wrote,
// never to the docker transcript (which carries no timestamp at all) or to
// the exit code.
func normalizeTimestamp(s string) string {
	return isoTimestampPattern.ReplaceAllString(s, "TIMESTAMP")
}

// fixturePlaceholder stands in for a outputFixture's own base directory
// wherever scrubFixturePath finds it.
const fixturePlaceholder = "FIXTURE"

// scrubFixturePath replaces every occurrence of base (a outputFixture's own
// root, itself under t.TempDir()) with fixturePlaceholder. Now that "want"
// can be a golden recorded from a past, separate process rather than a live
// run sharing this test's own fixture, the two sides no longer share one
// t.TempDir(): every path either prints (a compose --project-directory, a
// worktree's ROOT column, doctor's "control home" line) would otherwise
// differ on that basis alone. Applied to stdout, stderr and each Docker
// call; never to the exit code.
func scrubFixturePath(s, base string) string {
	return strings.ReplaceAll(s, base, fixturePlaceholder)
}

// fingerprintPattern matches the index page's own data-body attribute: a
// sha256 of the rendered rows (render.go's `fingerprint`), truncated and
// base64url-encoded. Those rows embed a checkout's absolute path (as a
// tooltip's title attribute), so scrubFixturePath changes the bytes the
// fingerprint was hashed over without being able to change the hash itself,
// which was already computed, by the build under test, before this file
// ever sees the page. Scrubbed for the same reason as the timestamp: it
// legitimately differs between two runs, by construction, not by exclusion.
var fingerprintPattern = regexp.MustCompile(`data-body="[A-Za-z0-9_-]{16}"`)

// toolVersionPattern matches doctor's "ok" line for Git: a subject whose
// version comes from a real `git` binary this test never controls, scrubbed
// to a placeholder for the same reason as the timestamp and the
// fingerprint — it legitimately differs between two runs, since each reads
// whatever git is actually installed on whichever machine ran it, and a
// golden recorded on one machine must still compare equal on any other.
// Written into the golden already scrubbed, not just at compare time, so
// re-recording it on a different runner (or after this machine's own next
// `apt upgrade`) is a no-op rather than spurious drift. Anchored on "ok" so
// a "fail" line's own message (“ `git` could not be started “, say) is never
// touched. Node.js is matched too, for a golden's own recorded line (see
// excludeStdoutLines below); doctor itself never prints one.
var toolVersionPattern = regexp.MustCompile(`(?m)^(\s*ok\s+(?:Git|Node\.js)\s+)\S+$`)

// jsonToolVersionPattern is toolVersionPattern for `doctor --json`, where
// the same Git version is the "detail" of the check whose "id" is
// "prerequisites.git", however the other keys are ordered or spaced.
// runIn fails a doctor --json run this does not match, so a changed layout
// is reported as that rather than as a Git version in a golden.
var jsonToolVersionPattern = regexp.MustCompile(`("id": "prerequisites\.git",[^}]*?"detail": )"[^"]*"`)

// Every case's expected output, frozen as a golden corpus under
// testdata/output/<name>/, and replayed against the live binary from here
// on: same paths, same UUIDs, the same fake docker every time. `down`,
// `worktree remove`, and `list --all --refresh` (#41, #204) all refresh the page, so
// every docker call each makes is compared, with no exclusion. The one
// thing that could never match byte for byte between two separate process
// invocations is real wall-clock time: normalizeTimestamp scrubs it,
// narrowly, everywhere a run's own output or the page it wrote could carry
// one.
//
// testdata/output/ is a plain, hand-maintained fixture corpus, the same way
// golden_test.go's own testdata/golden/ is: update a fixture by hand after a
// change to this build's own behaviour in one of these cases, the same way
// any other testdata file in this repository is updated.

const (
	fixtureRepo = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	fixtureWt   = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
)

type outputRun struct {
	Stdout, Stderr string
	Exit           int
	Docker         []string
	// IndexPage is the overview page on disk after the run, timestamp-
	// and fixture-path-normalized and with its content fingerprint scrubbed
	// (see fingerprintPattern), for a case that asks readIndexPage to read it
	// (down and worktree remove both refresh it as a side effect; "" for a
	// case that does not check it, so cases that never touch the index need
	// no change).
	IndexPage string
}

type outputFixture struct {
	base, checkout, controlHome, bin, state string
}

// newOutputFixture lays out a registered worktree at base, from scratch: a
// git checkout with fixed identity markers, a complete record, a home, and a
// fake docker holding one container and one volume of it.
func newOutputFixture(t *testing.T, base string, decoy bool) outputFixture {
	t.Helper()
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	f := outputFixture{base: base, checkout: filepath.Join(base, "checkout"), controlHome: filepath.Join(base, "control"),
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
	write(filepath.Join(f.checkout, "treerig.toml"), "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n[service.api]\n[service.mail]\nsubdomain = \"mail\"\n", 0o644)
	git := exec.Command("git", "init", "--initial-branch=feature/login", ".")
	git.Dir = f.checkout
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	write(filepath.Join(f.checkout, ".git", "treerig", "repository-id"), registry.IdentityMarkerContents(fixtureRepo), 0o600)
	write(filepath.Join(f.checkout, ".git", "treerig", "worktree-id"), registry.IdentityMarkerContents(fixtureWt), 0o600)
	paths := layout.WorktreeLayout(f.controlHome, fixtureRepo, fixtureWt)
	// This build's own marker format: files.AssertAdoptableControlHome only
	// cares that the marker exists, not its exact bytes, but this is what a
	// real control home actually has.
	write(filepath.Join(f.controlHome, ".treerig-control-home"), files.ControlHomeFormat, 0o600)
	// TREERIG_HOME is the control home in this fixture: the scratch root's own
	// port range lives beside the marker, so no run takes the default one (#119).
	registrytest.WriteScratchSettings(t, f.controlHome, "")
	write(paths.RecordPath, `{
  "version": 1,
  "repositoryId": "`+fixtureRepo+`",
  "worktreeId": "`+fixtureWt+`",
  "slug": "feature-login",
  "domain": "acme.localhost",
  "labelNamespace": "dev.treerig",
  "initialization": "complete"
}
`, 0o600)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	project := layout.ComposeProjectName(fixtureRepo, fixtureWt)
	owner := fixtureWt
	if decoy {
		owner = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	}
	// State is files: c1 (a container of api) and v1 (a volume) exist until removed.
	write(filepath.Join(f.bin, "docker"), `#!/bin/sh
state="`+f.state+`"
echo "$*" >> "$state/argv"
case " $* " in
  *" compose "*" config "*) echo '{"name":"x","services":{"api":{"image":"busybox"},"mail":{"image":"busybox"}}}'; exit 0 ;;
  *" --project-name treerig-router ps "*" --quiet "*) echo routerc1; exit 0 ;;
  *" inspect routerc1 "*) echo '[{"State":{"Status":"running"},"Config":{"Labels":{"`+router.RouterHomeLabel+`":"`+f.controlHome+`","`+router.RouterPortLabel+`":"80"}}}]'; exit 0 ;;
  *" compose "*" ps "*) [ -e "$state/c1" ] && echo '{"Service":"api","State":"running","Health":""}'; exit 0 ;;
  *" compose "*" down "*) rm -f "$state/c1"; exit 0 ;;
  *" --filter "*)
    case "$1" in
      ps) case " $* " in
            *"label=dev.treerig.managed=true --format"*) [ -e "$state/c1" ] && printf 'running\tUp 5 minutes\t`+fixtureRepo+`\t`+fixtureWt+`\tapi\t\tc1\n'; exit 0 ;;
            *"label=dev.treerig.shared=true"*) exit 0 ;;
            *"label=com.docker.compose.project="*"--format {{.State}}"*) [ -e "$state/c1" ] && echo running; exit 0 ;;
          esac; [ -e "$state/c1" ] && echo c1 ;;
      volume) [ -e "$state/v1" ] && echo v1 ;;
    esac
    exit 0 ;;
  *" --format "*)
    id=$(eval echo "\${$#}")
    case "$id" in
      c1) printf 'true\t`+project+`\t`+fixtureRepo+`\t`+fixtureWt+`\tapi\t1\n' ;;
      v1) printf 'true\t`+project+`\t`+fixtureRepo+`\t`+owner+`\t\t\n' ;;
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

// replaceInFakeDocker rewrites the fake docker script, failing the test when
// old is not in it.
func replaceInFakeDocker(t *testing.T, f outputFixture, old, replacement string) {
	t.Helper()
	script := filepath.Join(f.bin, "docker")
	contents, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(contents), old, replacement, 1)
	if changed == string(contents) {
		t.Fatalf("the fake docker has no %q", old)
	}
	if err := os.WriteFile(script, []byte(changed), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeFixtureCard writes the index card an `up` would have left, recording
// root as the directory the worktree was last seen in.
func writeFixtureCard(t *testing.T, f outputFixture, root string) {
	t.Helper()
	path := card.IndexCardPath(f.controlHome, fixtureRepo, fixtureWt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `{"version": 1, "project": "acme", "domain": "acme.localhost", "slug": "feature-login", "host": "feature-login.acme.localhost", ` +
		`"root": ` + strconv.Quote(root) + `, "branch": "feature/login", "labelNamespace": "dev.treerig", "services": [], "updatedAt": "2026-01-01T00:00:00.000Z"}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFixtureCardWithServices writes the index card an `up` of the fixture
// would have left: root as the directory the worktree was last seen in, and
// the api service it routed at the fixture's own host.
func writeFixtureCardWithServices(t *testing.T, f outputFixture, root string) {
	t.Helper()
	path := card.IndexCardPath(f.controlHome, fixtureRepo, fixtureWt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `{"version": 1, "project": "acme", "domain": "acme.localhost", "slug": "feature-login", "host": "feature-login.acme.localhost", ` +
		`"root": ` + strconv.Quote(root) + `, "branch": "feature/login", "labelNamespace": "dev.treerig", "services": [` +
		`{"name": "api", "url": "http://feature-login.acme.localhost", "label": "", "purpose": ""}], "updatedAt": "2026-01-01T00:00:00.000Z"}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dockerNotAnswering is the setup of a case whose Docker answers every call
// that lists containers with the daemon's own refusal.
func dockerNotAnswering(t *testing.T, f outputFixture) {
	t.Helper()
	script := filepath.Join(f.bin, "docker")
	contents, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	refusal := "echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?' >&2; exit 1 ;;\n"
	down := strings.Replace(string(contents), "case \" $* \" in\n",
		"case \" $* \" in\n  *\" compose \"*\" ps \"*) "+refusal+"  *\" ps \"*\"label=dev.treerig.managed=true\"*) "+refusal, 1)
	if down == string(contents) {
		t.Fatal("the fake docker's case statement was not found")
	}
	if err := os.WriteFile(script, []byte(down), 0o755); err != nil {
		t.Fatal(err)
	}
}

// excludeLines drops every line of stdout containing substr — doctor's own
// Node.js line, excluded from the comparison the way the overview
// refresh's docker calls are excluded from the transcript.
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

// environ is the environment every run of this fixture gets.
func (f outputFixture) environ() []string {
	return []string{
		"HOME=" + f.base,
		// TREERIG_CONTROL_HOME points directly at the control home (#80);
		// TREERIG_HOME is also set to it, which moves the router's own state
		// under f.controlHome/router too — readIndexPage below relies on
		// that.
		"TREERIG_HOME=" + f.controlHome, "TREERIG_CONTROL_HOME=" + f.controlHome,
		"PATH=" + f.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	}
}

// pinnedDigestPattern matches the name a pinned teardown binary is stored
// under, which is the sha256 of the build under test and so differs from
// one build to the next.
var pinnedDigestPattern = regexp.MustCompile(`(teardown/)[0-9a-f]{64}`)

// runIn executes command in dir — the fixture's own checkout for every case
// but doctor's "no checkout" one, which needs a directory with no treerig.toml
// above it; f.base (this fixture's own root, holding only checkout/, bin/,
// state/ and control/) is that — and reports what it did. checkIndexPage
// additionally reads the overview page HOME's router state directory
// holds afterward (a case whose verb refreshes the index as a side effect —
// down, worktree remove, or list --all --refresh); a case that never touches the
// index passes false, so IndexPage stays "" and contributes nothing to the
// comparison.
func (f outputFixture) runIn(t *testing.T, dir string, command []string, checkIndexPage bool) outputRun {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = f.environ()
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
		calls = append(calls, scrubFixturePath(line, f.base))
	}
	if slices.Contains(command, "doctor") && slices.Contains(command, "--json") && !jsonToolVersionPattern.MatchString(stdout.String()) {
		t.Fatalf("doctor --json has no check with id prerequisites.git and a detail; update jsonToolVersionPattern for the new layout:\n%s", stdout.String())
	}
	run := outputRun{
		Stdout: jsonToolVersionPattern.ReplaceAllString(
			toolVersionPattern.ReplaceAllString(scrubFixturePath(normalizeTimestamp(stdout.String()), f.base), "${1}VERSION"), `${1}"VERSION"`),
		Stderr: pinnedDigestPattern.ReplaceAllString(scrubFixturePath(normalizeTimestamp(stderr.String()), f.base), "${1}DIGEST"),
		Exit:   exit, Docker: calls,
	}
	if checkIndexPage {
		page := scrubFixturePath(normalizeTimestamp(string(f.readIndexPage(t))), f.base)
		run.IndexPage = fingerprintPattern.ReplaceAllString(page, `data-body="FINGERPRINT"`)
	}
	return run
}

// readIndexPage reads the overview page this fixture's last run
// wrote, resolved through layout.RouterState the same way runIn's own env
// does (HOME=f.base, TREERIG_HOME=f.controlHome). Each call to run() rebuilds
// the fixture from scratch first (see newOutputFixture), so "" means the
// case under test never wrote one.
func (f outputFixture) readIndexPage(t *testing.T) []byte {
	t.Helper()
	state, err := layout.RouterState(func(key string) (string, bool) {
		switch key {
		case "HOME":
			return f.base, true
		case "TREERIG_HOME":
			return f.controlHome, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("resolving the router state: %v", err)
	}
	page, err := os.ReadFile(state.IndexFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the index page: %v", err)
	}
	return page
}

// readOutputGolden reads back a frozen golden fixture, reconstructing the
// outputRun it recorded. IndexPage is only read for a case whose
// checkIndexPage flag is set; every other case leaves it "", matching a
// live run that never checked it either.
func readOutputGolden(t *testing.T, dir string, checkIndexPage bool) outputRun {
	t.Helper()
	run := outputRun{
		Stdout: readFixtureFile(t, filepath.Join(dir, "stdout")),
		Stderr: readFixtureFile(t, filepath.Join(dir, "stderr")),
		Exit:   readFixtureCode(t, filepath.Join(dir, "exit")),
	}
	if raw := strings.TrimSpace(readFixtureFile(t, filepath.Join(dir, "docker"))); raw != "" {
		run.Docker = strings.Split(raw, "\n")
	}
	if checkIndexPage {
		run.IndexPage = readFixtureFile(t, filepath.Join(dir, "indexpage"))
	}
	return run
}

func TestOutputGoldens(t *testing.T) {
	binary := buildBinary(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())

	cases := []struct {
		name  string
		args  []string
		decoy bool
		// outsideCheckout runs from the fixture's own base instead of its
		// checkout — doctor's "no checkout" case needs a directory with no
		// treerig.toml above it.
		outsideCheckout bool
		// excludeStdoutLines drops any stdout line containing one of these
		// substrings from both the golden and the live run before
		// comparing — doctor's Node.js line, recorded in the golden but
		// never printed by this build (there is no Go runtime version to
		// check), the same way the overview refresh's own docker
		// calls are excluded from the transcript above.
		excludeStdoutLines []string
		// unorderedDocker compares Docker as a set instead of a sequence:
		// doctor asks Docker Engine and Compose concurrently in both
		// builds, and which one's call lands in the transcript first is not
		// itself meaningful.
		unorderedDocker bool
		// checkIndexPage additionally compares the overview page on
		// disk (timestamp normalized) for a case whose verb refreshes it.
		checkIndexPage bool
		// excludeDockerCalls drops any Docker transcript entry containing one
		// of these substrings from both the golden and the live run before
		// comparing, filtering both sides the way excludeStdoutLines does.
		excludeDockerCalls []string
		// setup runs after newOutputFixture builds the checkout and before
		// the live binary does, for a case that needs something on disk
		// newOutputFixture does not already put there — configured hooks,
		// for "doctor with hooks configured" below.
		setup func(t *testing.T, f outputFixture)
	}{
		// status asks Compose for the services while it asks Docker whether the
		// router's container is up, so the order of those calls in the transcript
		// is not itself meaningful (#203): unorderedDocker compares them as a set.
		{name: "status", args: []string{"status"}, unorderedDocker: true},
		// The state words for a worktree whose removal was interrupted, and for
		// Docker not answering: still printed, the cause on stderr, and status
		// exits 1 for the daemon as it did before.
		{
			name: "list, an interrupted removal", args: []string{"list"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				marker := layout.WorktreeLayout(f.controlHome, fixtureRepo, fixtureWt).RemovalPath
				if err := os.WriteFile(marker, []byte(registry.RemovalMarkerContents()), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		// The URL column follows the router (#203): with its container gone, no URL
		// is shown and stderr says why.
		{
			name: "status, router stopped", args: []string{"status"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { replaceInFakeDocker(t, f, "echo routerc1; exit 0", "exit 0") },
		},
		{
			name: "status --json, router stopped", args: []string{"status", "--json"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { replaceInFakeDocker(t, f, "echo routerc1; exit 0", "exit 0") },
		},
		// A router on another port than this root's serves no URL of it.
		{
			name: "status, router on another port", args: []string{"status"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { replaceInFakeDocker(t, f, `"80"}}}]`, `"8080"}}}]`) },
		},
		// A worktree is named by its slug from a directory that is no repository:
		// its treerig.toml is read from the directory its card recorded.
		{
			name: "status of a named worktree, outside a repository", args: []string{"status", "feature-login"},
			outsideCheckout: true, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { writeFixtureCard(t, f, f.checkout) },
		},
		{
			name: "status --json of a named worktree whose directory is gone", args: []string{"status", "--json", "feature-login"},
			outsideCheckout: true,
			setup: func(t *testing.T, f outputFixture) {
				writeFixtureCard(t, f, filepath.Join(f.base, "gone"))
			},
		},
		{
			name: "status of a named worktree whose directory is gone", args: []string{"status", "feature-login"},
			outsideCheckout: true,
			setup: func(t *testing.T, f outputFixture) {
				writeFixtureCard(t, f, filepath.Join(f.base, "gone"))
			},
		},
		{
			name: "status of a worktree that is not registered", args: []string{"status"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				if err := os.Remove(layout.WorktreeLayout(f.controlHome, fixtureRepo, fixtureWt).RecordPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "status --json of a worktree that is not registered", args: []string{"status", "--json"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				if err := os.Remove(layout.WorktreeLayout(f.controlHome, fixtureRepo, fixtureWt).RecordPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "status, Docker not answering", args: []string{"status"}, setup: dockerNotAnswering, unorderedDocker: true},
		{name: "list, Docker not answering", args: []string{"list"}, setup: dockerNotAnswering, unorderedDocker: true},
		{name: "list --all --refresh, Docker not answering", args: []string{"list", "--all", "--refresh"}, setup: dockerNotAnswering, checkIndexPage: true, unorderedDocker: true},
		// urls, index and worktree list are hidden stubs that name their
		// replacement and exit 1 (#203, #204).
		{name: "urls", args: []string{"urls"}},
		{name: "index, a stub", args: []string{"index"}},
		{name: "index --json, a stub", args: []string{"index", "--json"}},
		{name: "worktree list, a stub", args: []string{"worktree", "list"}},
		{name: "worktree list --json, a stub", args: []string{"worktree", "list", "--json"}},
		// list --all --refresh's own stdout embeds snapshot.GeneratedAt, real
		// wall-clock time at the moment each build's own process ran;
		// normalizeTimestamp scrubs it (both here and in the page below)
		// rather than excluding the case, so the rest of its output and
		// every docker call it makes are still compared exactly.
		{name: "list --all --refresh", args: []string{"list", "--all", "--refresh"}, checkIndexPage: true, unorderedDocker: true},
		{name: "list", args: []string{"list"}, unorderedDocker: true},
		{name: "list --all", args: []string{"list", "--all"}, unorderedDocker: true},
		{name: "list --json", args: []string{"list", "--json"}, unorderedDocker: true},
		// The URL column follows the service and the router (#204): the card
		// `up` leaves names the service it routed, the fake container runs it,
		// and the router answers on this root's port.
		{
			name: "list, a running service's URL", args: []string{"list"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { writeFixtureCardWithServices(t, f, f.checkout) },
		},
		{
			name: "list --json, a running service's URL", args: []string{"list", "--json"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) { writeFixtureCardWithServices(t, f, f.checkout) },
		},
		{
			name: "list, router stopped", args: []string{"list"}, unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) {
				writeFixtureCardWithServices(t, f, f.checkout)
				replaceInFakeDocker(t, f, "echo routerc1; exit 0", "exit 0")
			},
		},
		// #27: the JSON forms of the four verbs above and doctor's, one
		// document each on stdout, the text form's exit code.
		{name: "status --json", args: []string{"status", "--json"}, unorderedDocker: true},
		{name: "urls --json", args: []string{"urls", "--json"}},
		{name: "list --all --refresh --json", args: []string{"list", "--all", "--refresh", "--json"}, checkIndexPage: true, unorderedDocker: true},
		{
			name: "doctor --json", args: []string{"doctor", "--json"}, unorderedDocker: true,
			outsideCheckout: true,
		},
		{name: "down", args: []string{"down"}, checkIndexPage: true},
		{name: "worktree remove --yes", args: []string{"worktree", "remove", "--yes"}, checkIndexPage: true},
		{name: "worktree remove without --yes, not a terminal", args: []string{"worktree", "remove"}},
		{name: "worktree remove --yes, refusing a decoy", args: []string{"worktree", "remove", "--yes"}, decoy: true},
		{name: "worktree remove of an unknown slug", args: []string{"worktree", "remove", "--yes", "nope"}},
		{name: "worktree remove --label-namespace that disagrees", args: []string{"worktree", "remove", "--yes", "--label-namespace", "dev.wrong"}},
		{name: "worktree remove --label-namespace that is not one", args: []string{"worktree", "remove", "--yes", "--label-namespace", "Not One"}},
		// worktree unlock: only the refusals that never reach a lock (an
		// unregistered identifier, or a worktree nothing has locked at all);
		// what a lock's holder says depends on live processes, so those cases
		// are covered by internal/lock's own tests instead.
		{name: "worktree unlock of an unknown identifier", args: []string{"worktree", "unlock", "nope"}},
		{name: "worktree unlock with nothing held", args: []string{"worktree", "unlock"}},
		// #200: the same verbs from a directory that is no repository and has no
		// treerig.toml above it. list shows every registered worktree, as
		// --all does; remove and unlock find their target by slug and need
		// nothing else; with no name there is nothing to act on.
		{name: "list outside a repository", args: []string{"list"}, outsideCheckout: true, unorderedDocker: true},
		{name: "list --json outside a repository", args: []string{"list", "--json"}, outsideCheckout: true, unorderedDocker: true},
		{name: "worktree remove --yes of a slug outside a repository", args: []string{"worktree", "remove", "--yes", "feature-login"}, outsideCheckout: true},
		{name: "worktree remove outside a repository, naming nothing", args: []string{"worktree", "remove", "--yes"}, outsideCheckout: true},
		{name: "worktree unlock of a slug outside a repository", args: []string{"worktree", "unlock", "feature-login"}, outsideCheckout: true},
		{name: "worktree unlock outside a repository, naming nothing", args: []string{"worktree", "unlock"}, outsideCheckout: true},
		// doctor: every other line (Prerequisites' Git/Docker checks,
		// Registry, Locks, Router, This checkout) is byte-identical once the
		// Node.js, treerig root and router state lines are excluded — the
		// last two postdate the golden recording (#80) — removing the
		// Node.js line does not even shift the Prerequisites column width,
		// since "Docker Compose" is still the longest subject either way.
		// Node.js is excluded whole because this build's own doctor never
		// prints that line at all (there is nothing in a live run to
		// compare it against); Git's line stays, but toolVersionPattern
		// above scrubs its version first, since the live run asks the real
		// `git` on whatever machine runs this test, which a frozen golden
		// cannot otherwise agree with on every future runner.
		{
			name: "doctor with no checkout", args: []string{"doctor"}, outsideCheckout: true,
			excludeStdoutLines: []string{
				"Node.js", "treerig root", "router state",
			},
			unorderedDocker: true,
		},
		// #105: hookResolutionChecks' own "This checkout" lines — recorded
		// against the current build, so unlike "doctor with no checkout"
		// above it needs no Node.js/treerig-root/router-state exclusions.
		// prepare resolves (a real, executable "./hooks/prepare" with a "#!"
		// line); seed does not (configured as "./hooks/seed", never written,
		// so it predicts the runner's own 127); verify stays unconfigured,
		// so "skip … none configured" still pins that unrelated line's own
		// wording and column width unchanged. Docker Engine and Compose are
		// asked concurrently, so unorderedDocker avoids pinning an
		// unmeaningful race between the two.
		{
			name: "doctor with hooks configured", args: []string{"doctor"},
			unorderedDocker: true,
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				hooksDir := filepath.Join(f.checkout, "hooks")
				if err := os.MkdirAll(hooksDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(hooksDir, "prepare"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				tomlPath := filepath.Join(f.checkout, "treerig.toml")
				contents, err := os.ReadFile(tomlPath)
				if err != nil {
					t.Fatal(err)
				}
				contents = append(contents, []byte("[hooks]\nprepare = \"./hooks/prepare\"\nseed = \"./hooks/seed\"\n")...)
				if err := os.WriteFile(tomlPath, contents, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		// #29: a state refusal's exact text, end to end. The first is a control
		// home whose marker a newer treerig wrote; the second a container whose
		// format label is a newer treerig's. Both name the store, the format found,
		// the format this build reads, and say to upgrade.
		{
			name: "worktree unlock --registry, control home from a newer treerig", args: []string{"worktree", "unlock", "--registry"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(f.controlHome, ".treerig-control-home"), []byte("format 3\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "status, an identity marker with no format line", args: []string{"status"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(f.checkout, ".git", "treerig", "worktree-id"), []byte(fixtureWt+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "worktree remove --yes, refusing a container with no format label", args: []string{"worktree", "remove", "--yes"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				script := filepath.Join(f.bin, "docker")
				contents, err := os.ReadFile(script)
				if err != nil {
					t.Fatal(err)
				}
				unlabelled := strings.Replace(string(contents), `\tapi\t1\n'`, `\tapi\t\n'`, 1)
				if unlabelled == string(contents) {
					t.Fatal("the fake docker's container line was not found")
				}
				if err := os.WriteFile(script, []byte(unlabelled), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "worktree remove --yes, refusing a container of a newer format", args: []string{"worktree", "remove", "--yes"},
			setup: func(t *testing.T, f outputFixture) {
				t.Helper()
				script := filepath.Join(f.bin, "docker")
				contents, err := os.ReadFile(script)
				if err != nil {
					t.Fatal(err)
				}
				newer := strings.Replace(string(contents), `\tapi\t1\n'`, `\tapi\t2\n'`, 1)
				if newer == string(contents) {
					t.Fatal("the fake docker's container line was not found")
				}
				if err := os.WriteFile(script, []byte(newer), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := filepath.Join(base, "fixture")
			dir := filepath.Join("testdata", "output", tc.name)

			want := readOutputGolden(t, dir, tc.checkIndexPage)

			gotFixture := newOutputFixture(t, fixture, tc.decoy)
			if tc.setup != nil {
				tc.setup(t, gotFixture)
			}
			// `worktree remove` runs only the pinned teardown binary (#146), so
			// its cases pin the build under test first; the goldens then carry
			// the gate's two lines, with the pinned binary's stored name
			// scrubbed.
			if len(tc.args) >= 2 && tc.args[0] == "worktree" && tc.args[1] == "remove" {
				pinWithEnv(t, binary, gotFixture.checkout, gotFixture.environ(), "golden-pin")
			}
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
				t.Errorf("golden (-) and live run (+) differ:\n%s", diff)
			}
		})
	}
}
