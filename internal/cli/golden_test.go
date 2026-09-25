package cli_test

// Golden corpus for the CLI's parsing, help and top-level error reporting,
// captured from the real TypeScript build (the oracle) and replayed against
// cli.Main.
//
// Regenerate after a change to the command tree, help wording or an error
// message, with Node 24 on PATH:
//
//	go test ./internal/cli/... -run TestGolden -update
//
// -update runs `node src/cli/main.ts <args>` for every case below in a
// clean temporary HOME/BERTH_HOME with no berth.toml on its cwd, and writes
// its stdout, stderr and exit code into testdata/golden/<name>/. Without
// -update the test only reads those files back — no Node needed to run it —
// and compares them byte for byte against cli.Main run in-process with the
// same args (argv[0] "berth", matching the oracle's own name when run as
// `node src/cli/main.ts`, whose basename resolves to "berth" — see
// brand.CLIName and cliName() in src/brand.ts).
//
// A case named in goOwned prints a surface only the Go build has, approved
// by the divergence-ledger row it names: -update records it from cli.Main
// instead of the oracle, so regenerating the corpus keeps it rather than
// overwriting it with TypeScript's output, and CI's oracle drift check
// stays clean.
//
// Every case here stays within this package's remit — parsing, help,
// suggestions and the report()/BERTH_DEBUG-hint path — so none of them
// depend on a command's own body. Two specific exceptions are documented at
// their case below.

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
)

var update = flag.Bool("update", false, "regenerate testdata/golden from the TypeScript CLI (needs node on PATH)")

type goldenCase struct {
	name string
	args []string
}

// goOwned names the cases recorded from the Go build rather than the
// oracle, each with the divergence-ledger row that approves the difference.
var goOwned = map[string]string{
	// Row 27: `worktree unlock --registry/--router`, which TypeScript lacks.
	"help-worktree":            "row 27",
	"help-worktree-short":      "row 27",
	"help-worktree-unlock":     "row 27",
	"help-cmd-worktree":        "row 27",
	"help-cmd-worktree-remove": "row 27",
	"help-cmd-worktree-list":   "row 27",
	"bare-worktree":            "row 27",
}

var goldenCases = []goldenCase{
	// --- help: the program and every group and subcommand ---
	{"help-root-long", []string{"--help"}},
	{"help-root-short", []string{"-h"}},
	{"help-root-command", []string{"help"}},
	{"help-config", []string{"config", "--help"}},
	{"help-config-short", []string{"config", "-h"}},
	{"help-config-validate", []string{"config", "validate", "--help"}},
	{"help-config-show", []string{"config", "show", "--help"}},
	{"help-dev", []string{"dev", "--help"}},
	{"help-up", []string{"up", "--help"}},
	{"help-down", []string{"down", "--help"}},
	{"help-status", []string{"status", "--help"}},
	{"help-urls", []string{"urls", "--help"}},
	{"help-index", []string{"index", "--help"}},
	{"help-doctor", []string{"doctor", "--help"}},
	{"help-router", []string{"router", "--help"}},
	{"help-router-short", []string{"router", "-h"}},
	{"help-router-up", []string{"router", "up", "--help"}},
	{"help-router-status", []string{"router", "status", "--help"}},
	{"help-router-down", []string{"router", "down", "--help"}},
	{"help-worktree", []string{"worktree", "--help"}},
	{"help-worktree-short", []string{"worktree", "-h"}},
	{"help-worktree-list", []string{"worktree", "list", "--help"}},
	{"help-worktree-remove", []string{"worktree", "remove", "--help"}},
	{"help-worktree-unlock", []string{"worktree", "unlock", "--help"}},

	// --- the "help [command]" form, including its one-word-only quirk ---
	{"help-cmd-config", []string{"help", "config"}},
	{"help-cmd-router", []string{"help", "router"}},
	{"help-cmd-worktree", []string{"help", "worktree"}},
	// commander's default help command takes a single, non-variadic
	// argument: the word after "worktree" is dropped, so this shows
	// worktree's own group help, not remove's.
	{"help-cmd-worktree-remove", []string{"help", "worktree", "remove"}},
	{"help-cmd-worktree-list", []string{"help", "worktree", "list"}},
	{"help-cmd-dev", []string{"help", "dev"}},
	// Same one-word rule: "b" is dropped, and since "a" is not a real
	// command this falls all the way back to the program's own error help.
	{"help-cmd-a-b", []string{"help", "a", "b"}},
	{"help-cmd-nonexistent", []string{"help", "nonexistent"}},

	// --- version ---
	{"version-short", []string{"-V"}},
	{"version-long", []string{"--version"}},

	// --- bare groups: help to stderr, exit 1 ---
	{"bare-root", nil},
	{"bare-config", []string{"config"}},
	{"bare-router", []string{"router"}},
	{"bare-worktree", []string{"worktree"}},

	// --- dev: implemented as a fixed "not implemented yet" failure ---
	{"dev-not-implemented", []string{"dev"}},

	// --- unknown command, with and without a suggestion ---
	{"err-unknown-command-far", []string{"nonsense"}},
	{"err-unknown-command-near-status", []string{"stat"}},
	{"err-unknown-command-near-router", []string{"route"}},
	{"err-unknown-command-near-worktree", []string{"wroktree"}},
	{"err-unknown-command-near-help", []string{"helo"}},
	{"err-unknown-command-near-down", []string{"dow"}},
	{"err-unknown-command-in-group", []string{"worktree", "bogus"}},
	{"err-unknown-command-in-config-group", []string{"config", "nonexistentSub"}},

	// --- unknown option, with and without a suggestion, short and long ---
	{"err-unknown-option-long", []string{"--frobnicate"}},
	{"err-unknown-option-long-suggest", []string{"--hepl"}},
	{"err-unknown-option-long-suggest-nested", []string{"worktree", "list", "--al"}},
	{"err-unknown-option-long-suggest-domain", []string{"worktree", "remove", "--domai", "foo"}},
	{"err-unknown-option-long-suggest-unlock", []string{"worktree", "unlock", "--holder-unverifable"}},
	{"err-unknown-option-short", []string{"-x"}},
	{"err-unknown-option-short-nested", []string{"worktree", "list", "-x"}},
	{"err-unknown-option-short-leaf", []string{"worktree", "remove", "-x"}},
	// "-hx": -h is never a recognised *option* (only the dedicated
	// --help/-h check matches it exactly), so this is one unrecognised
	// token, not -h followed by an unknown -x.
	{"err-unknown-option-short-combo", []string{"-hx"}},

	// --- missing option argument ---
	{"err-missing-option-arg-config", []string{"--config"}},
	{"err-missing-option-arg-domain", []string{"worktree", "remove", "--domain"}},
	{"err-missing-option-arg-label-namespace", []string{"worktree", "remove", "--label-namespace"}},

	// --- too many arguments ---
	{"err-excess-arguments-remove-one", []string{"worktree", "remove", "foo", "bar"}},
	{"err-excess-arguments-remove-two", []string{"worktree", "remove", "a", "b", "c"}},
	{"err-excess-arguments-status", []string{"status", "extra"}},

	// --- option-scanning edge cases ---
	// -V is a real registered option (unlike -h): the combo scan matches it
	// and exits on the version handler before "extra" is ever considered.
	{"combo-version-short-suffix", []string{"-Vextra"}},
	// --config is root-scoped and recognised wherever it appears in the
	// argument list, not just before the first subcommand name.
	{"config-after-subcommand-version", []string{"worktree", "list", "-V"}},
	{"config-after-subcommand-help", []string{"worktree", "list", "-h"}},
	{"config-inline-equals", []string{"--config=/nonexistent-berth-config-x7z.toml", "config", "validate"}},
	// A required option's value is whatever token follows, even one that
	// looks like another flag: --config eats "--help" here, leaving a bare
	// invocation (full help to stderr, exit 1) rather than showing help.
	{"config-eats-next-token", []string{"--config", "--help"}},

	// --- cases that reach an actual (now-ported) command body, to prove
	// --config really reaches commands.Env.ConfigPath for each of them.
	// Their exact wording belongs to the work package that ported the
	// command, not this one; they are included because the path is a
	// literal, fixed argument (not derived from a temp directory or the
	// test's own cwd), so the output stays deterministic across machines —
	// unlike, say, these same commands run with no --config at all, whose
	// "no berth.toml" message would embed wherever this repository happens
	// to be checked out. ---
	{"config-validate-missing-file", []string{"config", "validate", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"config-show-missing-file", []string{"config", "show", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"worktree-list-missing-file", []string{"worktree", "list", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"urls-missing-file", []string{"urls", "--config", "/nonexistent-berth-config-x7z.toml"}},
}

func TestGolden(t *testing.T) {
	if *update {
		requireOracleNode(t)
	}
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "golden", tc.name)
			if *update {
				var stdout, stderr string
				var code int
				if _, ok := goOwned[tc.name]; ok {
					stdout, stderr, code = runGo(tc.args)
				} else {
					stdout, stderr, code = runOracle(t, tc.args)
				}
				writeFixture(t, dir, stdout, stderr, code)
			}

			wantOut := readFixtureFile(t, filepath.Join(dir, "stdout"))
			wantErr := readFixtureFile(t, filepath.Join(dir, "stderr"))
			wantCode := readFixtureCode(t, filepath.Join(dir, "exit"))

			var stdout, stderr bytes.Buffer
			args := append([]string{"berth"}, tc.args...)
			code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, emptyGetenv)

			if stdout.String() != wantOut {
				t.Errorf("stdout mismatch\n--- want ---\n%s\n--- got ---\n%s", wantOut, stdout.String())
			}
			if stderr.String() != wantErr {
				t.Errorf("stderr mismatch\n--- want ---\n%s\n--- got ---\n%s", wantErr, stderr.String())
			}
			if code != wantCode {
				t.Errorf("exit code = %d, want %d", code, wantCode)
			}
		})
	}
}

// runGo runs cli.Main in-process the way TestGolden compares it, for
// recording a goOwned case.
func runGo(args []string) (stdout, stderr string, exitCode int) {
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{"berth"}, args...), strings.NewReader(""), &out, &errOut, emptyGetenv)
	return out.String(), errOut.String(), code
}

// requireOracleNode stops -update unless the node on PATH is the major
// version .node-version pins: another would record its own errors, or a
// different build's output, as every golden.
func requireOracleNode(t *testing.T) {
	t.Helper()
	pinned, err := os.ReadFile(filepath.Join("..", "..", ".node-version"))
	if err != nil {
		t.Fatal(err)
	}
	wantMajor, _, _ := strings.Cut(strings.TrimSpace(string(pinned)), ".")
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		t.Fatalf("-update needs node %s on PATH: %v", wantMajor, err)
	}
	gotMajor, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), ".")
	if gotMajor != wantMajor {
		t.Fatalf("-update needs node %s, as .node-version pins; the node on PATH is %s", wantMajor, strings.TrimSpace(string(out)))
	}
}

// emptyGetenv is a controlled, empty environment: nothing this package's
// own cases read (BERTH_DEBUG, HOME, BERTH_HOME) is ever set, matching the
// oracle's own controlled run.
func emptyGetenv(string) (string, bool) { return "", false }

// runOracle runs `node src/cli/main.ts <args>` the way the conventions doc
// specifies: a clean temporary HOME/BERTH_HOME, cwd a clean temp directory
// with no berth.toml above it, PATH trimmed to node's own directory (plus
// /usr/bin so node can still find a shell if it needs one), and COLUMNS
// left unset. Node's stdout/stderr are OS pipes, never a TTY, matching
// "stdout not a TTY".
func runOracle(t *testing.T, args []string) (stdout, stderr string, exitCode int) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node not on PATH (see this file's header comment): %v", err)
	}
	mainTS, err := filepath.Abs(filepath.Join("..", "..", "src", "cli", "main.ts"))
	if err != nil {
		t.Fatalf("resolving src/cli/main.ts: %v", err)
	}

	home := t.TempDir()
	cwd := t.TempDir()

	cmd := exec.Command(nodeBin, append([]string{mainTS}, args...)...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=" + filepath.Dir(nodeBin) + ":/usr/bin:/bin",
		"HOME=" + home,
		"BERTH_HOME=" + filepath.Join(home, "control-home"),
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running oracle (node %s %v): %v", mainTS, args, runErr)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// writeFixture writes the oracle's recorded stdout, stderr and exit code as
// the three files a golden fixture is made of, creating dir if needed.
func writeFixture(t *testing.T, dir, stdout, stderr string, code int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	writeFile(t, filepath.Join(dir, "stdout"), stdout)
	writeFile(t, filepath.Join(dir, "stderr"), stderr)
	writeFile(t, filepath.Join(dir, "exit"), strconv.Itoa(code)+"\n")
}

// writeFile writes content to path, failing the test if the write fails.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readFixtureFile reads a golden fixture file, failing the test with a hint
// to regenerate it with -update if it is missing.
func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update first? see this file's header comment): %v", path, err)
	}
	return string(data)
}

// readFixtureCode reads a golden fixture's exit-code file and parses it,
// failing the test if the contents aren't a valid integer.
func readFixtureCode(t *testing.T, path string) int {
	t.Helper()
	raw := strings.TrimSpace(readFixtureFile(t, path))
	code, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parsing %s as an exit code: %v", path, err)
	}
	return code
}
