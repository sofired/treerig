//go:build integration

package cli_test

// The streams contract: the CLI as a real process, because which stream a
// line lands on is only a contract if it survives the entry point — a unit
// test can prove a logger method was called, only a real pipe can prove
// nothing else wrote to it. CI runs this as part of "Integration tests"
// (go.yml), against a binary this package builds itself (buildBinary, in
// signal_integration_test.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/registry/registrytest"
)

// rtlOverride is U+202E, the right-to-left override: config show must escape
// it in the JSON it prints rather than pass it through raw, since nothing a
// service label carries may be able to drive the terminal displaying it.
// Spelled as a hex rune, not a \u escape or the literal character, so it
// reads unambiguously in a diff instead of as an invisible control byte.
const rtlOverride = rune(0x202e)

// streamsConfig is a minimal, valid berth.toml: one service, no hooks.
const streamsConfig = `
[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[service.api]
`

// invocation is one run of the CLI: what each stream carried and how it
// exited.
type invocation struct {
	Stdout, Stderr string
	Code           int
}

// berthOptions adjusts one run of runBerth from its defaults: the real PATH,
// no BERTH_DEBUG, no extra environment.
type berthOptions struct {
	// path overrides PATH; empty keeps the real one, as Node's own child
	// process would inherit it.
	path string
	// debug sets BERTH_DEBUG=1.
	debug bool
	// env sets or overrides further variables, applied after BERTH_HOME,
	// PATH and HOME so a case can override any of them too.
	env map[string]string
}

// runBerth runs binary with args in cwd, giving it BERTH_HOME under cwd and
// HOME set to cwd — never this process's own environment, so no run here
// can touch a real developer's registered worktrees or router — and reports
// its stdout, stderr and exit code.
func runBerth(t *testing.T, binary, cwd string, args []string, opts berthOptions) invocation {
	t.Helper()
	path := opts.path
	if path == "" {
		path = os.Getenv("PATH")
	}
	env := []string{
		"BERTH_HOME=" + filepath.Join(cwd, "control-home"),
		"PATH=" + path,
		"HOME=" + cwd,
	}
	if opts.debug {
		env = append(env, "BERTH_DEBUG=1")
	}
	for key, value := range opts.env {
		env = append(env, key+"="+value)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return invocation{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

// writeStreamsProject lays out a fresh directory with the given berth.toml
// content and a minimal compose.yaml (one "api" service backed by busybox).
func writeStreamsProject(t *testing.T, config string) string {
	t.Helper()
	dir := registrytest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  api:\n    image: busybox:1.37.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berth.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitRun runs a git subcommand in dir with the developer's own global and
// system git config disabled, the same isolation output_integration_test.go's
// runIn gives it, so a real commit.gpgsign on the machine running this test
// cannot fail it.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// gitInitCommit initializes dir as a git repository on feature/login with one
// empty commit — enough identity for the registry to adopt it, without
// needing any tracked file.
func gitInitCommit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "--initial-branch=feature/login", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "initial")
}

// emptyPATHDir returns a temporary directory with nothing in it, so a binary
// run with PATH set to it can find neither git nor docker.
func emptyPATHDir(t *testing.T) string {
	t.Helper()
	return registrytest.TempDir(t)
}

func TestStreamsContract(t *testing.T) {
	binary := buildBinary(t)

	t.Run("puts a validator report on stdout and keeps stderr empty", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)

		result := runBerth(t, binary, cwd, []string{"config", "validate"}, berthOptions{})

		if result.Code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr=%q", result.Code, result.Stderr)
		}
		if result.Stderr != "" {
			t.Errorf("stderr = %q, want empty", result.Stderr)
		}
		if !strings.Contains(result.Stdout, "berth.toml is valid.") {
			t.Errorf("stdout = %q, want it to contain %q", result.Stdout, "berth.toml is valid.")
		}
		if !strings.Contains(result.Stdout, "project acme · domain acme.localhost · 1 service(s) · 0 hook(s)") {
			t.Errorf("stdout = %q, missing the summary line", result.Stdout)
		}
	})

	t.Run("counts every configured hook", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig+`
[hooks]
prepare = "./prepare.sh"
seed    = "./seed.sh"
verify  = "./verify.sh"
backup  = "./backup.sh"
`)

		result := runBerth(t, binary, cwd, []string{"config", "validate"}, berthOptions{})

		if !strings.Contains(result.Stdout, "1 service(s) · 4 hook(s)") {
			t.Errorf("stdout = %q, want the 4-hook(s) count", result.Stdout)
		}
	})

	t.Run("prints JSON a terminal cannot be driven by, that parses back exactly", func(t *testing.T) {
		wantLabel := "API" + string(rtlOverride)
		cwd := writeStreamsProject(t, streamsConfig+
			"[service.api.index]\n"+
			`label   = "`+wantLabel+"\"\n"+
			`purpose = "surface"`+"\n")

		result := runBerth(t, binary, cwd, []string{"config", "show"}, berthOptions{})

		if result.Stderr != "" {
			t.Errorf("stderr = %q, want empty", result.Stderr)
		}
		if regexp.MustCompile(`[\x7f-\x{ffff}]`).MatchString(result.Stdout) {
			t.Errorf("stdout has a raw non-ASCII or control byte, want every one escaped: %q", result.Stdout)
		}
		wantEscaped := fmt.Sprintf(`\u%04x`, rtlOverride)
		if !strings.Contains(result.Stdout, wantEscaped) {
			t.Errorf("stdout = %q, want it to contain the escaped RTL override %s", result.Stdout, wantEscaped)
		}
		var parsed struct {
			Service []struct {
				Index *struct {
					Label string `json:"label"`
				} `json:"index"`
			} `json:"service"`
		}
		if err := json.Unmarshal([]byte(result.Stdout), &parsed); err != nil {
			t.Fatalf("parsing stdout as JSON: %v\nstdout=%q", err, result.Stdout)
		}
		if len(parsed.Service) == 0 || parsed.Service[0].Index == nil || parsed.Service[0].Index.Label != wantLabel {
			t.Fatalf("parsed = %+v, want service[0].index.label = %q", parsed, wantLabel)
		}
	})

	t.Run("keeps a failure, and what it names, off stdout", func(t *testing.T) {
		cwd := registrytest.TempDir(t) // no berth.toml anywhere above this directory

		result := runBerth(t, binary, cwd, []string{"config", "validate"}, berthOptions{})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "No berth.toml found") {
			t.Errorf("stderr = %q, want it to contain %q", result.Stderr, "No berth.toml found")
		}
		if !strings.Contains(result.Stderr, "berth.example.toml") {
			t.Errorf("stderr = %q, want it to point at berth.example.toml", result.Stderr)
		}
		if strings.Contains(result.Stderr, "berth init") {
			t.Errorf("stderr = %q, must not suggest a command this build has no init for", result.Stderr)
		}
	})

	t.Run("filters the stack trace it prints when asked for one", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		// The path is echoed back in the failure, and with BERTH_DEBUG=1 it is
		// echoed again inside the stack trace, printed outside the logger.
		hostile := filepath.Join(cwd, "missing\x1b[2K.toml")

		result := runBerth(t, binary, cwd, []string{"--config", hostile, "config", "validate"}, berthOptions{debug: true})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "�") {
			t.Errorf("stderr = %q, want the escape sequence replaced by U+FFFD", result.Stderr)
		}
		if strings.ContainsRune(result.Stderr, '\x1b') {
			t.Errorf("stderr = %q, must not carry a raw ESC byte through to the terminal", result.Stderr)
		}
	})

	t.Run("prints the doctor's report on stdout and how it went on stderr", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		empty := emptyPATHDir(t) // neither git nor docker can be found

		result := runBerth(t, binary, cwd, []string{"doctor"}, berthOptions{path: empty})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if !regexp.MustCompile(`^Prerequisites\n`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want it to open with the Prerequisites section", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^ {2}fail {2}Docker Engine +the ` + "`docker`" + ` command is not on PATH`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want a failed Docker Engine check", result.Stdout)
		}
		if !regexp.MustCompile("(?m)^ {2}fail {2}Git +`git` could not be started").MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want a failed Git check", result.Stdout)
		}
		if !strings.Contains(result.Stdout, "This checkout") {
			t.Errorf("stdout = %q, want the This checkout section", result.Stdout)
		}
		if strings.Contains(result.Stdout, "[berth]") {
			t.Errorf("stdout = %q, must not carry the logger's own prefix", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^\[berth\] ERROR: \d+ check\(s\) failed\.$`).MatchString(result.Stderr) {
			t.Errorf("stderr = %q, want the failure summary", result.Stderr)
		}
	})

	t.Run("says on stderr, as information, that nothing failed, and exits 0", func(t *testing.T) {
		cwd := registrytest.TempDir(t)
		bin := registrytest.TempDir(t)
		// Just enough of a Docker and a Git to pass: versions to report, and
		// no router or routed containers to find.
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(strings.Join([]string{
			"#!/bin/sh",
			`if [ "$1" = version ]; then echo 29.0.0; fi`,
			`if [ "$1" = info ]; then echo '{"OSType":"linux","OperatingSystem":"Ubuntu","Name":"fake","SecurityOptions":[]}'; fi`,
			`if [ "$1" = context ]; then echo unix:///var/run/docker.sock; fi`,
			`if [ "$1" = compose ] && [ "$2" = version ]; then echo 2.30.0; fi`,
			"exit 0", "",
		}, "\n")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"git version 2.43.0\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		// The fake engine is a Linux one, which berth on any other client OS
		// cannot place, so the router mode is set in runBerth's berth root.
		registrytest.WriteScratchSettings(t, filepath.Join(cwd, "control-home"), "[router]\nmode = \"host\"\n")

		result := runBerth(t, binary, cwd, []string{"doctor"}, berthOptions{path: bin})

		if result.Code != 0 {
			t.Errorf("exit code = %d, want 0\nstdout=%q\nstderr=%q", result.Code, result.Stdout, result.Stderr)
		}
		if !regexp.MustCompile(`(?m)^ {2}ok {4}Docker Compose +2\.30\.0$`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want an ok Docker Compose check", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^ {2}ok {4}router +not running`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want an ok router check", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^\[berth\] No checks failed \(1 skipped\)\.$`).MatchString(result.Stderr) {
			t.Errorf("stderr = %q, want the no-failures notice", result.Stderr)
		}
		if strings.Contains(result.Stderr, "ERROR") {
			t.Errorf("stderr = %q, must not say ERROR when nothing failed", result.Stderr)
		}
	})

	t.Run("reports a router port it cannot use as a failed check, and still reports the rest", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)

		result := runBerth(t, binary, cwd, []string{"doctor"}, berthOptions{env: map[string]string{"BERTH_ROUTER_PORT": "eighty"}})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if !regexp.MustCompile(`(?m)^ {2}fail {2}router +BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "eighty"\.$`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want the failed router-port check", result.Stdout)
		}
		if !strings.Contains(result.Stdout, "This checkout") {
			t.Errorf("stdout = %q, want the rest of the report to still print", result.Stdout)
		}
	})

	t.Run("keeps worktree unlock off stdout: a refusal is an error, nothing held is a notice", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		gitInitCommit(t, cwd)

		neverUp := runBerth(t, binary, cwd, []string{"worktree", "unlock"}, berthOptions{})
		if neverUp.Code != 1 {
			t.Errorf("exit code = %d, want 1", neverUp.Code)
		}
		if neverUp.Stdout != "" {
			t.Errorf("stdout = %q, want empty", neverUp.Stdout)
		}
		if !strings.Contains(neverUp.Stderr, "This checkout has never been brought up, so it holds no lock. Name another with 'berth worktree unlock <slug|uuid>'.") {
			t.Errorf("stderr = %q, want the never-up refusal", neverUp.Stderr)
		}

		unknown := runBerth(t, binary, cwd, []string{"worktree", "unlock", "nobody"}, berthOptions{})
		if unknown.Code != 1 {
			t.Errorf("exit code = %d, want 1", unknown.Code)
		}
		if unknown.Stdout != "" {
			t.Errorf("stdout = %q, want empty", unknown.Stdout)
		}
		if !strings.Contains(unknown.Stderr, "No worktree is registered as nobody.") {
			t.Errorf("stderr = %q, want the unknown-identifier refusal", unknown.Stderr)
		}
	})

	t.Run("leaves stdout empty when the answer is a notice, not data", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		gitInitCommit(t, cwd)

		result := runBerth(t, binary, cwd, []string{"status"}, berthOptions{})

		if result.Code != 0 {
			t.Errorf("exit code = %d, want 0", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "Not registered") {
			t.Errorf("stderr = %q, want it to say Not registered", result.Stderr)
		}
	})

	t.Run("pipes a machine-readable list with nothing else in it", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		gitInitCommit(t, cwd)

		result := runBerth(t, binary, cwd, []string{"worktree", "list", "--json"}, berthOptions{})

		if result.Code != 0 {
			t.Errorf("exit code = %d, want 0", result.Code)
		}
		if result.Stderr != "" {
			t.Errorf("stderr = %q, want empty", result.Stderr)
		}
		var list []any
		if err := json.Unmarshal([]byte(result.Stdout), &list); err != nil {
			t.Fatalf("parsing stdout as JSON: %v\nstdout=%q", err, result.Stdout)
		}
		if len(list) != 0 {
			t.Errorf("list = %v, want empty", list)
		}
	})

	t.Run("keeps a failed up off stdout, and says on stderr what it could not reach", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		gitInitCommit(t, cwd)
		// Git, so `up` gets as far as registering; no docker, so it can go no further.
		onlyGit := registrytest.TempDir(t)
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(gitPath, filepath.Join(onlyGit, "git")); err != nil {
			t.Fatal(err)
		}

		result := runBerth(t, binary, cwd, []string{"up", "api"}, berthOptions{path: onlyGit})

		if result.Code == 0 {
			t.Errorf("exit code = 0, want a failure")
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !regexp.MustCompile(`(?im)^\[berth\] ERROR: .*docker`).MatchString(result.Stderr) {
			t.Errorf("stderr = %q, want it to name docker", result.Stderr)
		}
	})

	t.Run("reports an unreachable router as a failure rather than as output", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		empty := emptyPATHDir(t) // no docker to be found, which is what an unreachable router looks like from here

		result := runBerth(t, binary, cwd, []string{"router", "status"}, berthOptions{path: empty})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "Router state unknown") {
			t.Errorf("stderr = %q, want it to say so", result.Stderr)
		}
	})

	// The README's install step is `ln -s <binary> somewhere-on-PATH`, and the
	// rename story is that the symlink's own name is what people see —
	// brand.CLIName reads argv[0], which must survive being invoked by a
	// different name.
	t.Run("answers to the name it was symlinked as, and to the product name otherwise", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		link := filepath.Join(cwd, "duke")
		if err := os.Symlink(binary, link); err != nil {
			t.Fatal(err)
		}

		// Through runBerth, not a hand-built exec.Command: HOME and
		// BERTH_HOME must stay scoped to cwd here too, or this run reaches
		// past its own fixture into whatever this machine's real developer
		// has registered.
		named := runBerth(t, link, cwd, []string{"--help"}, berthOptions{})
		if !strings.Contains(named.Stdout, "Usage:\n  duke [flags]\n  duke [command]") {
			t.Errorf("stdout = %q, want the symlinked name in the usage line", named.Stdout)
		}

		byPath := runBerth(t, binary, cwd, []string{"--help"}, berthOptions{})
		if !strings.Contains(byPath.Stdout, "Usage:\n  berth [flags]\n  berth [command]") {
			t.Errorf("stdout = %q, want the product name in the usage line", byPath.Stdout)
		}
	})
}
