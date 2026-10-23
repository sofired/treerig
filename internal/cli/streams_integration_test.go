//go:build integration

package cli_test

// The streams contract: the CLI as a real process, because which stream a
// line lands on is only a contract if it survives the entry point — a unit
// test can prove a logger method was called, only a real pipe can prove
// nothing else wrote to it. CI runs this as part of "Integration tests"
// (go.yml), against a binary this package builds itself (buildBinary, in
// signal_integration_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/registry/registrytest"
)

// rtlOverride is U+202E, the right-to-left override: config show must escape
// it in the JSON it prints rather than pass it through raw, since nothing a
// service label carries may be able to drive the terminal displaying it.
// Spelled as a hex rune, not a \u escape or the literal character, so it
// reads unambiguously in a diff instead of as an invisible control byte.
const rtlOverride = rune(0x202e)

// streamsConfig is a minimal, valid treerig.toml: one service, no hooks.
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

// treerigOptions adjusts one run of runTreerig from its defaults: the real PATH,
// no TREERIG_DEBUG, no extra environment.
type treerigOptions struct {
	// path overrides PATH; empty keeps the real one, as Node's own child
	// process would inherit it.
	path string
	// debug sets TREERIG_DEBUG=1.
	debug bool
	// env sets or overrides further variables, applied after TREERIG_HOME,
	// PATH and HOME so a case can override any of them too.
	env map[string]string
	// argv0 is the name the process is started as, as a symlink such as
	// `trig` would give it; empty keeps the binary's own path.
	argv0 string
	// timeout, when set, kills a run that takes longer; a gate that exec-ed
	// itself in a loop would otherwise hang the suite.
	timeout time.Duration
}

// runTreerig runs binary with args in cwd, giving it TREERIG_HOME under cwd and
// HOME set to cwd — never this process's own environment, so no run here
// can touch a real developer's registered worktrees or router — and reports
// its stdout, stderr and exit code.
func runTreerig(t *testing.T, binary, cwd string, args []string, opts treerigOptions) invocation {
	t.Helper()
	path := opts.path
	if path == "" {
		path = os.Getenv("PATH")
	}
	env := []string{
		"TREERIG_HOME=" + filepath.Join(cwd, "control-home"),
		"PATH=" + path,
		"HOME=" + cwd,
	}
	if opts.debug {
		env = append(env, "TREERIG_DEBUG=1")
	}
	for key, value := range opts.env {
		env = append(env, key+"="+value)
	}
	ctx := context.Background()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if opts.argv0 != "" {
		cmd.Args[0] = opts.argv0
	}
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

// writeStreamsProject lays out a fresh directory with the given treerig.toml
// content and a minimal compose.yaml (one "api" service backed by busybox).
func writeStreamsProject(t *testing.T, config string) string {
	t.Helper()
	dir := registrytest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  api:\n    image: busybox:1.37.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "treerig.toml"), []byte(config), 0o644); err != nil {
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

		result := runTreerig(t, binary, cwd, []string{"config", "validate"}, treerigOptions{})

		if result.Code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr=%q", result.Code, result.Stderr)
		}
		if result.Stderr != "" {
			t.Errorf("stderr = %q, want empty", result.Stderr)
		}
		if !strings.Contains(result.Stdout, "treerig.toml is valid.") {
			t.Errorf("stdout = %q, want it to contain %q", result.Stdout, "treerig.toml is valid.")
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

		result := runTreerig(t, binary, cwd, []string{"config", "validate"}, treerigOptions{})

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

		result := runTreerig(t, binary, cwd, []string{"config", "show"}, treerigOptions{})

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
		cwd := registrytest.TempDir(t) // no treerig.toml anywhere above this directory

		result := runTreerig(t, binary, cwd, []string{"config", "validate"}, treerigOptions{})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "No treerig.toml found") {
			t.Errorf("stderr = %q, want it to contain %q", result.Stderr, "No treerig.toml found")
		}
		if !strings.Contains(result.Stderr, "treerig.example.toml") {
			t.Errorf("stderr = %q, want it to point at treerig.example.toml", result.Stderr)
		}
		if !strings.Contains(result.Stderr, "Run 'treerig init' at the root of the repository") {
			t.Errorf("stderr = %q, want it to suggest treerig init", result.Stderr)
		}
	})

	t.Run("init writes a starter that config validate accepts, and reports on stderr only", func(t *testing.T) {
		repo := registrytest.TempDir(t)
		gitInitCommit(t, repo)
		sub := filepath.Join(repo, "services", "api")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  api:\n    image: busybox:1.37.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		result := runTreerig(t, binary, sub, []string{"init"}, treerigOptions{})

		if result.Code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr=%q", result.Code, result.Stderr)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "Wrote "+filepath.Join(repo, "treerig.toml")+".") {
			t.Errorf("stderr = %q, want it to name the file written at the repository root", result.Stderr)
		}
		if strings.Contains(result.Stderr, "WARNING") {
			t.Errorf("stderr = %q, want no warning: the Compose file was found", result.Stderr)
		}
		written, err := os.ReadFile(filepath.Join(repo, "treerig.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(written), "\nfile = \"docker-compose.yml\"\n") {
			t.Errorf("treerig.toml does not name the Compose file found:\n%s", written)
		}

		validated := runTreerig(t, binary, sub, []string{"config", "validate"}, treerigOptions{})

		if validated.Code != 0 || validated.Stderr != "" {
			t.Fatalf("config validate: exit %d, stderr=%q; want 0 and empty", validated.Code, validated.Stderr)
		}
		if !strings.Contains(validated.Stdout, "treerig.toml is valid.") {
			t.Errorf("stdout = %q, want it to say treerig.toml is valid", validated.Stdout)
		}
	})

	t.Run("init refuses to overwrite a treerig.toml, writing nothing to stdout", func(t *testing.T) {
		repo := registrytest.TempDir(t)
		gitInitCommit(t, repo)
		existing := filepath.Join(repo, "treerig.toml")
		if err := os.WriteFile(existing, []byte("# mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		result := runTreerig(t, binary, repo, []string{"init"}, treerigOptions{})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "[treerig] ERROR: "+existing+" already exists; not overwriting it.") {
			t.Errorf("stderr = %q, want the refusal", result.Stderr)
		}
		if got, _ := os.ReadFile(existing); string(got) != "# mine\n" {
			t.Errorf("treerig.toml = %q, want it untouched", got)
		}
	})

	t.Run("init writes at this checkout's root even when GIT_DIR and GIT_WORK_TREE name another repository", func(t *testing.T) {
		here, other := registrytest.TempDir(t), registrytest.TempDir(t)
		gitInitCommit(t, here)
		gitInitCommit(t, other)

		result := runTreerig(t, binary, here, []string{"init"}, treerigOptions{
			env: map[string]string{"GIT_DIR": filepath.Join(other, ".git"), "GIT_WORK_TREE": other},
		})

		if result.Code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr=%q", result.Code, result.Stderr)
		}
		if _, err := os.Stat(filepath.Join(here, "treerig.toml")); err != nil {
			t.Errorf("no treerig.toml at this checkout's root: %v", err)
		}
		if _, err := os.Stat(filepath.Join(other, "treerig.toml")); err == nil {
			t.Error("wrote treerig.toml into the repository GIT_DIR named")
		}
	})

	t.Run("init tells you to install Git, exiting 127, when there is none on PATH", func(t *testing.T) {
		dir := registrytest.TempDir(t)

		result := runTreerig(t, binary, dir, []string{"init"}, treerigOptions{path: emptyPATHDir(t)})

		if result.Code != 127 {
			t.Errorf("exit code = %d, want 127", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "[treerig] ERROR: Git was not found on PATH. Install Git, then run 'treerig init' again.") {
			t.Errorf("stderr = %q, want the install hint", result.Stderr)
		}
	})

	t.Run("init refuses outside a git repository, writing nothing to stdout", func(t *testing.T) {
		dir := registrytest.TempDir(t)

		result := runTreerig(t, binary, dir, []string{"init"}, treerigOptions{})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !strings.Contains(result.Stderr, "[treerig] ERROR: "+dir+" is not inside a Git checkout (fatal: not a git repository") {
			t.Errorf("stderr = %q, want the refusal", result.Stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "treerig.toml")); err == nil {
			t.Error("wrote a treerig.toml outside a repository")
		}
	})

	t.Run("filters the stack trace it prints when asked for one", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		// The path is echoed back in the failure, and with TREERIG_DEBUG=1 it is
		// echoed again inside the stack trace, printed outside the logger.
		hostile := filepath.Join(cwd, "missing\x1b[2K.toml")

		result := runTreerig(t, binary, cwd, []string{"config", "validate", "--config", hostile}, treerigOptions{debug: true})

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

		result := runTreerig(t, binary, cwd, []string{"doctor"}, treerigOptions{path: empty})

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
		if strings.Contains(result.Stdout, "[treerig]") {
			t.Errorf("stdout = %q, must not carry the logger's own prefix", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^\[treerig\] ERROR: \d+ check\(s\) failed\.$`).MatchString(result.Stderr) {
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

		// The fake engine is a Linux one, which treerig on any other client OS
		// cannot place, so the router mode is set in runTreerig's treerig root.
		registrytest.WriteScratchSettings(t, filepath.Join(cwd, "control-home"), "[router]\nmode = \"host\"\n")

		result := runTreerig(t, binary, cwd, []string{"doctor"}, treerigOptions{path: bin})

		if result.Code != 0 {
			t.Errorf("exit code = %d, want 0\nstdout=%q\nstderr=%q", result.Code, result.Stdout, result.Stderr)
		}
		if !regexp.MustCompile(`(?m)^ {2}ok {4}Docker Compose +2\.30\.0$`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want an ok Docker Compose check", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^ {2}ok {4}router +not running`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want an ok router check", result.Stdout)
		}
		if !regexp.MustCompile(`(?m)^\[treerig\] No checks failed \(1 warning\(s\), 1 skipped\)\.$`).MatchString(result.Stderr) {
			t.Errorf("stderr = %q, want the no-failures notice", result.Stderr)
		}
		// The one warning is that nothing is pinned for `worktree remove`; it is
		// a warning and not a failure, so doctor still exits 0.
		if !strings.Contains(result.Stdout, "  warn  teardown pin   none pinned; `treerig worktree remove` will refuse until you pin one with ") {
			t.Errorf("stdout = %q, want the warning about the missing teardown pin", result.Stdout)
		}
		if strings.Contains(result.Stderr, "ERROR") {
			t.Errorf("stderr = %q, must not say ERROR when nothing failed", result.Stderr)
		}
	})

	t.Run("reports a router port it cannot use as a failed check, and still reports the rest", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)

		result := runTreerig(t, binary, cwd, []string{"doctor"}, treerigOptions{env: map[string]string{"TREERIG_ROUTER_PORT": "eighty"}})

		if result.Code != 1 {
			t.Errorf("exit code = %d, want 1", result.Code)
		}
		if !regexp.MustCompile(`(?m)^ {2}fail {2}router +TREERIG_ROUTER_PORT must be a port number between 1 and 65535, not "eighty"\.$`).MatchString(result.Stdout) {
			t.Errorf("stdout = %q, want the failed router-port check", result.Stdout)
		}
		if !strings.Contains(result.Stdout, "This checkout") {
			t.Errorf("stdout = %q, want the rest of the report to still print", result.Stdout)
		}
	})

	t.Run("runs worktree list and worktree unlock outside a repository, and still refuses a --config that is not there", func(t *testing.T) {
		cwd := registrytest.TempDir(t) // no Git repository and no treerig.toml anywhere above this directory

		list := runTreerig(t, binary, cwd, []string{"worktree", "list"}, treerigOptions{})
		if list.Code != 0 || list.Stdout != "" || list.Stderr != "[treerig] No worktrees are registered on this machine.\n" {
			t.Errorf("worktree list = %+v, want the empty-machine notice on stderr and nothing on stdout", list)
		}
		listJSON := runTreerig(t, binary, cwd, []string{"worktree", "list", "--json"}, treerigOptions{})
		if listJSON.Code != 0 || strings.TrimSpace(listJSON.Stdout) != "[]" || listJSON.Stderr != "" {
			t.Errorf("worktree list --json = %+v, want an empty array", listJSON)
		}

		unknown := runTreerig(t, binary, cwd, []string{"worktree", "unlock", "nobody"}, treerigOptions{})
		if unknown.Code != 1 || unknown.Stdout != "" || unknown.Stderr != "[treerig] ERROR: No worktree is registered as nobody.\n" {
			t.Errorf("worktree unlock nobody = %+v", unknown)
		}
		bare := runTreerig(t, binary, cwd, []string{"worktree", "unlock"}, treerigOptions{})
		if bare.Code != 1 || bare.Stdout != "" || !strings.Contains(bare.Stderr, "There is no worktree to unlock here: ") || strings.Contains(bare.Stderr, "No treerig.toml") {
			t.Errorf("worktree unlock = %+v", bare)
		}

		missing := filepath.Join(cwd, "nowhere", "treerig.toml")
		for _, args := range [][]string{{"worktree", "list"}, {"worktree", "unlock", "nobody"}, {"worktree", "remove", "--yes", "--dev-teardown", "nobody"}} {
			result := runTreerig(t, binary, cwd, append(args, "--config", missing), treerigOptions{})
			if result.Code != 1 || result.Stdout != "" || !strings.HasSuffix(result.Stderr, "ERROR: No treerig.toml at "+missing+".\n") {
				t.Errorf("%v --config <missing> = %+v", args, result)
			}
		}

		// Inside a Git repository treerig has never seen, a treerig.toml that
		// does not parse is set aside with a warning, and the command lists that
		// repository's worktrees (none), pointing at --all.
		repo := writeStreamsProject(t, "not = [valid")
		gitInitCommit(t, repo)
		inRepo := runTreerig(t, binary, repo, []string{"worktree", "list"}, treerigOptions{})
		if inRepo.Code != 0 || inRepo.Stdout != "" ||
			!strings.Contains(inRepo.Stderr, "[treerig] WARNING: Ignoring treerig.toml for this command: ") ||
			!strings.Contains(inRepo.Stderr, "[treerig] No worktrees are registered for this repository.\n") ||
			!strings.HasSuffix(inRepo.Stderr, "[treerig] Every repository: 'treerig worktree list --all'\n") {
			t.Errorf("worktree list in a repository with a broken treerig.toml = %+v", inRepo)
		}

		// Names complete too: an empty registry offers none, and says so with
		// the directive alone.
		complete := runTreerig(t, binary, cwd, []string{"__complete", "worktree", "remove", ""}, treerigOptions{})
		if complete.Code != 0 || complete.Stdout != ":36\n" {
			t.Errorf("__complete worktree remove = %+v", complete)
		}
	})

	t.Run("keeps worktree unlock off stdout: a refusal is an error, nothing held is a notice", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		gitInitCommit(t, cwd)

		neverUp := runTreerig(t, binary, cwd, []string{"worktree", "unlock"}, treerigOptions{})
		if neverUp.Code != 1 {
			t.Errorf("exit code = %d, want 1", neverUp.Code)
		}
		if neverUp.Stdout != "" {
			t.Errorf("stdout = %q, want empty", neverUp.Stdout)
		}
		if !strings.Contains(neverUp.Stderr, "This worktree has never been brought up, so it holds no lock. Name another with 'treerig worktree unlock <slug|uuid>'.") {
			t.Errorf("stderr = %q, want the never-up refusal", neverUp.Stderr)
		}

		unknown := runTreerig(t, binary, cwd, []string{"worktree", "unlock", "nobody"}, treerigOptions{})
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

		result := runTreerig(t, binary, cwd, []string{"status"}, treerigOptions{})

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

		result := runTreerig(t, binary, cwd, []string{"worktree", "list", "--json"}, treerigOptions{})

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

		result := runTreerig(t, binary, cwd, []string{"up", "api"}, treerigOptions{path: onlyGit})

		if result.Code == 0 {
			t.Errorf("exit code = 0, want a failure")
		}
		if result.Stdout != "" {
			t.Errorf("stdout = %q, want empty", result.Stdout)
		}
		if !regexp.MustCompile(`(?im)^\[treerig\] ERROR: .*docker`).MatchString(result.Stderr) {
			t.Errorf("stderr = %q, want it to name docker", result.Stderr)
		}
	})

	t.Run("reports an unreachable router as a failure rather than as output", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		empty := emptyPATHDir(t) // no docker to be found, which is what an unreachable router looks like from here

		result := runTreerig(t, binary, cwd, []string{"router", "status"}, treerigOptions{path: empty})

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

	// docs/install.md ("Calling it something else") tells people to
	// `ln -s <binary> somewhere-on-PATH`, and the rename story is that the
	// symlink's own name is what people see — brand.CLIName reads argv[0],
	// which must survive being invoked by a different name.
	t.Run("answers to the name it was symlinked as, and to the product name otherwise", func(t *testing.T) {
		cwd := writeStreamsProject(t, streamsConfig)
		link := filepath.Join(cwd, "duke")
		if err := os.Symlink(binary, link); err != nil {
			t.Fatal(err)
		}

		// Through runTreerig, not a hand-built exec.Command: HOME and
		// TREERIG_HOME must stay scoped to cwd here too, or this run reaches
		// past its own fixture into whatever this machine's real developer
		// has registered.
		named := runTreerig(t, link, cwd, []string{"--help"}, treerigOptions{})
		if !strings.Contains(named.Stdout, "Usage:\n  duke [flags]\n  duke [command]") {
			t.Errorf("stdout = %q, want the symlinked name in the usage line", named.Stdout)
		}

		byPath := runTreerig(t, binary, cwd, []string{"--help"}, treerigOptions{})
		if !strings.Contains(byPath.Stdout, "Usage:\n  treerig [flags]\n  treerig [command]") {
			t.Errorf("stdout = %q, want the product name in the usage line", byPath.Stdout)
		}
	})
}
