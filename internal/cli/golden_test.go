package cli_test

// Golden corpus for the CLI's parsing, help and top-level error reporting.
// Every fixture under testdata/golden/<name>/ (stdout, stderr, exit) is a
// plain, hand-maintained corpus: update one by hand, or by running this
// file's own TestGolden with -update (see the flag's own doc comment), after
// a change to the command tree, help wording or an error message — the same
// way any other testdata file in this repository is updated. Either way,
// review every diff yourself: -update writes whatever cli.Main happened to
// produce, not what it should have.
//
// help-route*, bare-route, complete-route and the err-*-route* cases pin
// `route add|rm|ls` (#6).
//
// help-shared*, bare-shared, complete-shared* and the err-*-shared* cases pin
// `shared up|down|status` (#123); help-worktree-unlock also shows `--shared`.
//
// help-worktree, help-worktree-short, help-worktree-unlock,
// help-cmd-worktree, help-cmd-worktree-remove, help-cmd-worktree-list and
// bare-worktree show `worktree unlock --registry`/`--router` (#21). help-run,
// help-cmd-run and the err-run-* and run-* cases pin `run` (#94).
//
// Every case here stays within this package's remit — parsing, help,
// suggestions and the report()/BERTH_DEBUG-hint path — so none of them
// depend on a command's own body. Two specific exceptions are documented at
// their case below.

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
)

// update, when passed as -update to `go test`, writes each case's observed
// stdout, stderr and exit code over its committed fixture instead of
// comparing against it. It never judges whether the observed output is
// correct — that is for whoever runs it to review in the diff it leaves
// before committing the result.
var update = flag.Bool("update", false, "write golden fixtures from the observed output instead of checking them")

type goldenCase struct {
	name string
	args []string
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
	{"help-config-schema", []string{"config", "schema", "--help"}},
	{"help-dev", []string{"dev", "--help"}},
	{"help-up", []string{"up", "--help"}},
	{"help-down", []string{"down", "--help"}},
	{"help-status", []string{"status", "--help"}},
	{"help-urls", []string{"urls", "--help"}},
	{"help-run", []string{"run", "--help"}},
	{"help-index", []string{"index", "--help"}},
	{"help-doctor", []string{"doctor", "--help"}},
	{"help-router", []string{"router", "--help"}},
	{"help-router-short", []string{"router", "-h"}},
	{"help-router-up", []string{"router", "up", "--help"}},
	{"help-router-status", []string{"router", "status", "--help"}},
	{"help-router-down", []string{"router", "down", "--help"}},
	{"help-shared", []string{"shared", "--help"}},
	{"help-shared-up", []string{"shared", "up", "--help"}},
	{"help-shared-down", []string{"shared", "down", "--help"}},
	{"help-shared-status", []string{"shared", "status", "--help"}},
	{"help-route", []string{"route", "--help"}},
	{"help-route-add", []string{"route", "add", "--help"}},
	{"help-route-rm", []string{"route", "rm", "--help"}},
	{"help-route-ls", []string{"route", "ls", "--help"}},
	{"help-worktree", []string{"worktree", "--help"}},
	{"help-worktree-short", []string{"worktree", "-h"}},
	{"help-worktree-list", []string{"worktree", "list", "--help"}},
	{"help-worktree-remove", []string{"worktree", "remove", "--help"}},
	{"help-worktree-unlock", []string{"worktree", "unlock", "--help"}},

	// --- the "help [command]" form, walking the full path given ---
	{"help-cmd-config", []string{"help", "config"}},
	{"help-cmd-config-schema", []string{"help", "config", "schema"}},
	{"help-cmd-router", []string{"help", "router"}},
	{"help-cmd-shared", []string{"help", "shared"}},
	{"help-cmd-shared-down", []string{"help", "shared", "down"}},
	{"help-cmd-route", []string{"help", "route"}},
	{"help-cmd-route-add", []string{"help", "route", "add"}},
	{"help-cmd-worktree", []string{"help", "worktree"}},
	{"help-cmd-worktree-remove", []string{"help", "worktree", "remove"}},
	{"help-cmd-worktree-list", []string{"help", "worktree", "list"}},
	{"help-cmd-dev", []string{"help", "dev"}},
	{"help-cmd-run", []string{"help", "run"}},
	// "a" is not a real command, so this falls all the way back to the
	// program's own error help; "b" is never reached.
	{"help-cmd-a-b", []string{"help", "a", "b"}},
	{"help-cmd-nonexistent", []string{"help", "nonexistent"}},
	{"help-cmd-worktree-bogus", []string{"help", "worktree", "bogus"}},
	{"help-cmd-leaf-extra", []string{"help", "status", "extra"}},

	// --- version ---
	{"version-short", []string{"-V"}},
	{"version-long", []string{"--version"}},

	// --- bare groups: help to stderr, exit 1 ---
	{"bare-root", nil},
	{"bare-config", []string{"config"}},
	{"bare-router", []string{"router"}},
	{"bare-route", []string{"route"}},
	{"bare-shared", []string{"shared"}},
	{"bare-worktree", []string{"worktree"}},

	// --- dev: implemented as a fixed "not implemented yet" failure ---
	{"dev-not-implemented", []string{"dev"}},

	// --- shell completion: the scripts themselves are cobra's, so only the
	// group, one shell's help and the requests a script sends are pinned ---
	{"bare-completion", []string{"completion"}},
	{"help-completion-zsh", []string{"completion", "zsh", "--help"}},
	{"err-unknown-command-in-completion-group", []string{"completion", "tcsh"}},
	{"complete-root", []string{"__complete", ""}},
	{"complete-config", []string{"__complete", "config", ""}},
	{"complete-route", []string{"__complete", "route", ""}},
	{"complete-route-add-flags", []string{"__complete", "route", "add", "--"}},
	{"complete-route-rm-no-config", []string{"__complete", "route", "rm", "--config", "/nonexistent-berth-config-x7z.toml", ""}},
	{"complete-route-rm-name-given", []string{"__complete", "route", "rm", "web", ""}},
	{"complete-shared", []string{"__complete", "shared", ""}},
	{"complete-shared-down-flags", []string{"__complete", "shared", "down", "--"}},
	{"complete-shared-up-no-config", []string{"__complete", "shared", "up", "--config", "/nonexistent-berth-config-x7z.toml", ""}},
	{"complete-worktree", []string{"__complete", "worktree", ""}},
	{"complete-worktree-remove-flags", []string{"__complete", "worktree", "remove", "--"}},
	{"complete-no-descriptions", []string{"__completeNoDesc", "router", ""}},
	{"complete-no-file-arguments", []string{"__complete", "worktree", "remove", ""}},
	{"complete-no-file-flag-value", []string{"__complete", "worktree", "remove", "--domain", ""}},
	{"complete-config-file", []string{"__complete", "status", "--config", ""}},
	{"complete-run-config-missing", []string{"__complete", "run", "--config", "/nonexistent-berth-config-x7z.toml", ""}},

	// --- unknown command, with and without a suggestion ---
	{"err-unknown-command-far", []string{"nonsense"}},
	{"err-unknown-command-near-status", []string{"stat"}},
	{"err-unknown-command-near-router", []string{"rooter"}},
	{"err-unknown-command-near-worktree", []string{"wroktree"}},
	// No suggestion: cobra never suggests the help command itself.
	{"err-unknown-command-near-help", []string{"helo"}},
	{"err-unknown-command-near-down", []string{"dow"}},
	{"err-unknown-command-in-group", []string{"worktree", "bogus"}},
	{"err-unknown-command-in-route", []string{"route", "ad"}},
	{"err-route-add-no-name", []string{"route", "add", "--port", "5173"}},
	{"err-route-add-two-names", []string{"route", "add", "web", "api", "--port", "5173"}},
	{"err-route-add-no-port", []string{"route", "add", "web"}},
	{"err-route-rm-no-name", []string{"route", "rm"}},
	{"err-unknown-option-route-add", []string{"route", "add", "web", "--prt", "5173"}},
	{"err-excess-arguments-route-ls", []string{"route", "ls", "now"}},
	{"route-ls-missing-file", []string{"route", "ls", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"err-unknown-command-in-shared", []string{"shared", "dwon"}},
	{"err-unknown-option-shared-up", []string{"shared", "up", "--recrate"}},
	{"err-excess-arguments-shared-status", []string{"shared", "status", "now"}},
	{"shared-up-missing-file", []string{"shared", "up", "--config", "/nonexistent-berth-config-x7z.toml"}},
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
	// "-hx": pflag reads a clustered short flag left to right, so "-h"
	// itself is recognised before "x" is found not to be — the shorthand
	// cluster fails on "x", not on the flag as a whole.
	{"err-unknown-option-short-combo", []string{"-hx"}},

	// --- missing option argument ---
	{"err-missing-option-arg-config", []string{"--config"}},
	{"err-missing-option-arg-domain", []string{"worktree", "remove", "--domain"}},
	{"err-missing-option-arg-label-namespace", []string{"worktree", "remove", "--label-namespace"}},

	// --- run: anything but the name before "--" is refused, however it is
	// spelled; the arguments after "--" are the command's own ---
	{"err-run-args-before-dash", []string{"run", "plan", "extra"}},
	{"err-run-flag-shorthand", []string{"run", "plan", "-auto-approve"}},
	{"err-run-flag-long", []string{"run", "plan", "--var=x"}},
	{"err-run-flag-before-name", []string{"run", "--var", "x", "plan"}},
	{"err-run-args-before-dash-and-after", []string{"run", "plan", "extra", "more", "--", "-auto-approve"}},

	// --- too many arguments ---
	{"err-excess-arguments-remove-one", []string{"worktree", "remove", "foo", "bar"}},
	{"err-excess-arguments-remove-two", []string{"worktree", "remove", "a", "b", "c"}},
	{"err-excess-arguments-status", []string{"status", "extra"}},
	{"err-excess-arguments-config-schema", []string{"config", "schema", "extra"}},
	{"err-excess-arguments-router-down", []string{"router", "down", "now", "please"}},

	// --- option-scanning edge cases ---
	// "-Vextra": pflag reads a clustered short flag left to right too, the
	// same as "-hx" above — "-V" is consumed, then the cluster fails on "e",
	// exit 1 to stderr rather than printing the version, exit 0 to stdout.
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

	// --- cases that reach an actual command body, to prove --config really
	// reaches commands.Env.ConfigPath for each of them. Their exact wording
	// belongs to that command's own tests, not this one; they are included
	// here because the path is a literal, fixed argument (not derived from
	// a temp directory or the test's own cwd), so the output stays
	// deterministic across machines —
	// unlike, say, these same commands run with no --config at all, whose
	// "no berth.toml" message would embed wherever this repository happens
	// to be checked out. ---
	{"config-validate-missing-file", []string{"config", "validate", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"config-show-missing-file", []string{"config", "show", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"worktree-list-missing-file", []string{"worktree", "list", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"urls-missing-file", []string{"urls", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"run-missing-file", []string{"run", "plan", "--config", "/nonexistent-berth-config-x7z.toml"}},
	{"run-missing-file-after-dash", []string{"run", "--config", "/nonexistent-berth-config-x7z.toml", "plan", "--", "--config", "elsewhere"}},
}

// TestGolden replays every goldenCases entry against cli.Main and compares
// its stdout, stderr and exit code byte for byte against the committed
// fixture under testdata/golden/<name>/.
func TestGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "golden", tc.name)

			var stdout, stderr bytes.Buffer
			args := append([]string{"berth"}, tc.args...)
			code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, emptyGetenv)

			if *update {
				writeFixture(t, dir, stdout.String(), stderr.String(), code)
				return
			}

			wantOut := readFixtureFile(t, filepath.Join(dir, "stdout"))
			wantErr := readFixtureFile(t, filepath.Join(dir, "stderr"))
			wantCode := readFixtureCode(t, filepath.Join(dir, "exit"))

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

// writeFixture overwrites dir's stdout, stderr and exit files with the
// observed output of one -update run, creating dir if this is a fixture's
// first run.
func writeFixture(t *testing.T, dir, stdout, stderr string, code int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	writeFixtureFile(t, filepath.Join(dir, "stdout"), stdout)
	writeFixtureFile(t, filepath.Join(dir, "stderr"), stderr)
	writeFixtureFile(t, filepath.Join(dir, "exit"), strconv.Itoa(code)+"\n")
}

// writeFixtureFile writes one golden fixture file, failing the test if it
// cannot be written.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// emptyGetenv is a controlled, empty environment: nothing this package's
// own cases read (BERTH_DEBUG, HOME, BERTH_HOME) is ever set.
func emptyGetenv(string) (string, bool) { return "", false }

// readFixtureFile reads a golden fixture file, failing the test with a hint
// that it is missing if it cannot be read.
func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (a committed fixture is missing? see this file's header comment): %v", path, err)
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
