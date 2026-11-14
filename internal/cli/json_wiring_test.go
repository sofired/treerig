package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/cli"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// jsonWiringFixture puts the process in an unregistered git checkout whose
// PATH holds git and nothing else, so Docker is unavailable and the treerig
// root is a scratch directory. It returns the getenv cli.Main should see.
func jsonWiringFixture(t *testing.T) func(string) (string, bool) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is needed: %v", err)
	}
	root := registrytest.TempDir(t)
	bin := filepath.Join(root, "bin")
	checkout := filepath.Join(root, "checkout")
	for _, dir := range []string{bin, checkout} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	toml := "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n[service.api]\n"
	if err := os.WriteFile(filepath.Join(checkout, "treerig.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "init", "--initial-branch=main", ".")
	git.Dir = checkout
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Chdir(checkout)
	home := filepath.Join(root, "treerig-home")
	values := map[string]string{"HOME": root, "TREERIG_HOME": home, "TREERIG_CONTROL_HOME": filepath.Join(home, "control")}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// decodeOneDocument fails the test unless stdout is exactly one JSON
// document, and decodes it into value.
func decodeOneDocument(t *testing.T, stdout string, value any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%q", err, stdout)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout holds more than one document (second decode: %v): %q", err, stdout)
	}
}

// TestJSONOutputThroughTheProgram runs the four --json verbs through
// cli.Main: stdout is one parseable document, and the exit code and stderr
// are the text form's.
func TestJSONOutputThroughTheProgram(t *testing.T) {
	getenv := jsonWiringFixture(t)
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := cli.Main(append([]string{"treerig"}, args...), strings.NewReader(""), &stdout, &stderr, getenv)
		return code, stdout.String(), stderr.String()
	}

	t.Run("status --json reports an unregistered worktree, exit 0, notice on stderr", func(t *testing.T) {
		code, stdout, stderr := run("status", "--json")
		var report commands.StatusReport
		decodeOneDocument(t, stdout, &report)
		if code != 0 || report.Registered || len(report.Services) != 0 {
			t.Fatalf("code = %d, report = %+v", code, report)
		}
		if !strings.Contains(stderr, "Not registered") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("urls --json keeps the refusal on stderr, exit 1, and prints no document", func(t *testing.T) {
		code, stdout, stderr := run("urls", "--json")
		if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "[treerig] ERROR: This worktree is not registered.") {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})

	t.Run("doctor --json prints its document and still exits non-zero on a failure", func(t *testing.T) {
		code, stdout, stderr := run("doctor", "--json")
		var report commands.DoctorReport
		decodeOneDocument(t, stdout, &report)
		if code != 1 || report.OK || report.ExitCode != 1 || report.Counts.Fail == 0 {
			t.Fatalf("code = %d, report counts = %+v", code, report.Counts)
		}
		if !strings.Contains(stderr, "check(s) failed") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("index --json prints the snapshot, with services unchecked, and exits 1, when Docker is not there", func(t *testing.T) {
		code, stdout, _ := run("index", "--json")
		var report commands.IndexReport
		decodeOneDocument(t, stdout, &report)
		if code != 1 || report.DockerReachable || report.DockerError == nil {
			t.Fatalf("code = %d, report = %+v", code, report)
		}
	})
}
