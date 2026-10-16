package cli_test

// Golden corpus for `teardown pin --file` and `teardown status` (#85): the
// plan, the success line, a digest mismatch, and `status` and `status --json`
// in each state. Every fixture under testdata/teardown/<name>/ (stdout,
// stderr, exit) is hand-maintained in the way golden_test.go describes, and
// reviewed by hand after a change to either command's output. The cases run in
// process against a scratch treerig root, with the scratch directory, the
// platform and the clock scrubbed from what they print, so the same fixture
// holds on Linux and macOS.
//
//	pin-file-plan                  the confirmation screen and the success line, typed on a terminal
//	pin-file-yes                   --yes: no plan, the success line only
//	pin-file-sha-mismatch          the digest given is not the file's
//	pin-file-not-executable        the digest matches, but the file is empty, not an executable
//	status-no-pin[-json]           nothing pinned: empty stdout (or {"pinned": false}), exit 1
//	status-valid[-json]            an intact file pin: exit 0
//	status-binary-missing[-json]   the stored binary is gone: the report, then the refusal
//	status-binary-mismatch[-json]  the stored binary has other bytes
//	status-foreign[-json]          a pin this build did not write: empty stdout, the foreign text
//	status-newer[-json]            a pin of a newer format: empty stdout, "Upgrade treerig"

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/cli"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/teardown/teardowntest"
)

var (
	// platformPattern is a pin's platform key, which names the machine the
	// tests run on.
	platformPattern = regexp.MustCompile(`(linux|darwin)_(amd64|arm64)`)
	// clockPattern is a pinned_at time, which is the moment the setup pinned.
	clockPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
)

// teardownGolden is a scratch treerig root and a local binary to pin.
type teardownGolden struct {
	base, home, treerigHome, file string
	content                       []byte
	digest                        string
}

// newTeardownGolden lays out a scratch root: HOME, a treerig root beside it and
// a local binary.
func newTeardownGolden(t *testing.T) teardownGolden {
	t.Helper()
	base := registrytest.TempDir(t)
	g := teardownGolden{
		base: base, home: filepath.Join(base, "home"), treerigHome: filepath.Join(base, "treerig-home"),
		file: filepath.Join(base, "bin", "treerig-local"), content: teardowntest.Executable(runtimePlatform(), " a treerig build to pin"),
	}
	if err := os.MkdirAll(g.home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(g.file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.file, g.content, 0o755); err != nil {
		t.Fatal(err)
	}
	g.digest = binstore.Hash(g.content)
	return g
}

// getenv is the scratch environment: no variable but the two homes.
func (g teardownGolden) getenv(key string) (string, bool) {
	switch key {
	case "HOME":
		return g.home, true
	case "TREERIG_HOME":
		return g.treerigHome, true
	}
	return "", false
}

// control is the scratch control home's layout.
func (g teardownGolden) control() layout.Control {
	return layout.ControlLayout(filepath.Join(g.treerigHome, "control-home"))
}

// runtimePlatform is the platform key of the machine running the tests, whose
// executable format the fixtures take.
func runtimePlatform() string { return teardown.PlatformKey(runtime.GOOS, runtime.GOARCH) }

// scrub replaces what differs between machines and runs: the scratch directory,
// the fixture's digest (the fixture is an ELF or a Mach-O header, by platform),
// the platform and the clock.
func (g teardownGolden) scrub(s string) string {
	s = strings.ReplaceAll(s, g.base, "BASE")
	s = strings.ReplaceAll(s, g.digest, "DIGEST")
	s = platformPattern.ReplaceAllString(s, "PLATFORM")
	return clockPattern.ReplaceAllString(s, "TIMESTAMP")
}

// run is cli.Main in the scratch environment, with the output scrubbed.
func (g teardownGolden) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Main(append([]string{"treerig"}, args...), strings.NewReader(""), &out, &errOut, g.getenv)
	return g.scrub(out.String()), g.scrub(errOut.String()), code
}

// pin makes the file pin the cases start from, failing the test if it cannot.
func (g teardownGolden) pin(t *testing.T) {
	t.Helper()
	if _, stderr, code := g.run(t, "teardown", "pin", "--file", g.file, "--sha256", g.digest, "--release", "my-build", "--yes"); code != 0 {
		t.Fatalf("pin: exit %d, stderr %q", code, stderr)
	}
}

// writePin replaces pin.json with body, creating the teardown directory.
func (g teardownGolden) writePin(t *testing.T, body string) {
	t.Helper()
	control := g.control()
	if err := os.MkdirAll(control.TeardownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control.PinPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// compareTeardownGolden checks (or, with -update, writes) one fixture.
func compareTeardownGolden(t *testing.T, name, stdout, stderr string, code int) {
	t.Helper()
	dir := filepath.Join("testdata", "teardown", name)
	if *update {
		writeFixture(t, dir, stdout, stderr, code)
		return
	}
	if want := readFixtureFile(t, filepath.Join(dir, "stdout")); stdout != want {
		t.Errorf("stdout mismatch\n--- want ---\n%s\n--- got ---\n%s", want, stdout)
	}
	if want := readFixtureFile(t, filepath.Join(dir, "stderr")); stderr != want {
		t.Errorf("stderr mismatch\n--- want ---\n%s\n--- got ---\n%s", want, stderr)
	}
	if want := readFixtureCode(t, filepath.Join(dir, "exit")); code != want {
		t.Errorf("exit code = %d, want %d", code, want)
	}
}

// TestTeardownPinGolden replays the pin cases.
func TestTeardownPinGolden(t *testing.T) {
	t.Run("pin-file-yes", func(t *testing.T) {
		g := newTeardownGolden(t)
		stdout, stderr, code := g.run(t, "teardown", "pin", "--file", g.file, "--sha256", g.digest, "--release", "my-build", "--yes")
		compareTeardownGolden(t, "pin-file-yes", stdout, stderr, code)
	})

	t.Run("pin-file-sha-mismatch", func(t *testing.T) {
		g := newTeardownGolden(t)
		wrong := binstore.Hash([]byte("not the file"))
		stdout, stderr, code := g.run(t, "teardown", "pin", "--file", g.file, "--sha256", wrong, "--release", "my-build", "--yes")
		compareTeardownGolden(t, "pin-file-sha-mismatch", stdout, stderr, code)
	})

	t.Run("pin-file-not-executable", func(t *testing.T) {
		g := newTeardownGolden(t)
		// An empty file is refused the same way on every platform; a script,
		// a foreign architecture and the rest are internal/teardown's tests.
		empty := []byte{}
		if err := os.WriteFile(g.file, empty, 0o755); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := g.run(t, "teardown", "pin", "--file", g.file, "--sha256", binstore.Hash(empty), "--release", "my-build", "--yes")
		compareTeardownGolden(t, "pin-file-not-executable", stdout, stderr, code)
	})

	// The plan is shown only on a terminal, which cli.Main cannot give a test:
	// the command runs in process with an Env that has one, writing to the same
	// logger the CLI would. Nothing echoes the typed answer here, so the
	// success line follows the "> " prompt on one line.
	t.Run("pin-file-plan", func(t *testing.T) {
		g := newTeardownGolden(t)
		var stdout, stderr bytes.Buffer
		env := &commands.Env{
			CommandName: "treerig", Logger: logger.NewConsole("treerig", &stdout, &stderr), Getenv: g.getenv,
			OS: runtime.GOOS, Arch: runtime.GOARCH, Cwd: g.base,
			Stdin: strings.NewReader("pin teardown binary\n"), StdinIsTTY: true, Stderr: &stderr,
		}
		err := commands.TeardownPin(context.Background(), env, commands.TeardownPinOptions{
			File: g.file, SHA256: g.digest, Release: "my-build",
			Now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) },
		})
		if err != nil {
			t.Fatal(err)
		}
		compareTeardownGolden(t, "pin-file-plan", g.scrub(stdout.String()), g.scrub(stderr.String()), 0)
	})
}

// TestTeardownStatusGolden replays the status cases, text and JSON.
func TestTeardownStatusGolden(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, g teardownGolden)
	}{
		{"status-no-pin", func(*testing.T, teardownGolden) {}},
		{"status-valid", func(t *testing.T, g teardownGolden) { g.pin(t) }},
		{"status-binary-missing", func(t *testing.T, g teardownGolden) {
			g.pin(t)
			if err := os.Remove(filepath.Join(g.control().TeardownDir, g.digest)); err != nil {
				t.Fatal(err)
			}
		}},
		{"status-binary-mismatch", func(t *testing.T, g teardownGolden) {
			g.pin(t)
			if err := os.WriteFile(filepath.Join(g.control().TeardownDir, g.digest), []byte("other bytes\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"status-foreign", func(t *testing.T, g teardownGolden) { g.writePin(t, `{"version": 1, "bogus": true}`) }},
		{"status-newer", func(t *testing.T, g teardownGolden) { g.writePin(t, `{"version": 2}`) }},
	}
	for _, tc := range cases {
		for _, json := range []bool{false, true} {
			name, args := tc.name, []string{"teardown", "status"}
			if json {
				name, args = name+"-json", append(args, "--json")
			}
			t.Run(name, func(t *testing.T) {
				g := newTeardownGolden(t)
				tc.setup(t, g)
				stdout, stderr, code := g.run(t, args...)
				compareTeardownGolden(t, name, stdout, stderr, code)
			})
		}
	}
}
