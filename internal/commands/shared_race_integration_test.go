//go:build integration

package commands_test

// Two processes, two worktrees of one repository, both bringing up their first
// `up` at the same moment against a real daemon (docs/design/routing.md,
// decision 18): the shared lock makes one of them start the shared service and
// the other reuse it, and both end connected. Each `up` runs in its own
// process because the lock is a flock, and it is the processes' own file
// descriptions it tells apart. The test binary re-executes itself as the
// helper, chosen by an environment variable, as the lock and lifecycle suites do.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run/osrun"
)

// The helper's environment: where to run, where to say it is ready, what to
// wait for, where to report.
const (
	helperMode   = "TREERIG_TEST_SHARED_UP_HELPER"
	helperCwd    = "TREERIG_TEST_HELPER_CWD"
	helperGo     = "TREERIG_TEST_HELPER_GO"
	helperReady  = "TREERIG_TEST_HELPER_READY"
	helperReport = "TREERIG_TEST_HELPER_REPORT"
)

// TestMain runs the suite, or, when the helper variable is set, one `up`.
func TestMain(m *testing.M) {
	if os.Getenv(helperMode) == "1" {
		os.Exit(runUpHelper())
	}
	os.Exit(m.Run())
}

// runUpHelper reports ready, waits for the go file, runs `up` in the working
// directory it was
// given with a fake router (the config declares no routed service) against the
// real daemon, and writes every line it logged to the report file. It exits 0
// on success.
func runUpHelper() int {
	// The barrier both processes start behind: a file the parent writes once
	// both have reported ready, by writing their own ready file just before
	// they begin polling for it, so neither can be still starting up when the
	// other is released. Polling a file with a short sleep is the whole
	// synchronisation here (no signal or pipe is needed, and the sleep orders
	// nothing: the shared lock is what the test is about), and a loop that
	// sleeps is what a helper must do (it never parks in select {}).
	if err := os.WriteFile(os.Getenv(helperReady), []byte("ready"), 0o600); err != nil {
		return 1
	}
	for {
		if _, err := os.Stat(os.Getenv(helperGo)); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	log := &logger.Recording{}
	env := &commands.Env{
		CommandName: "treerig", Invocation: "treerig up", Logger: log, Environ: os.Environ(),
		Getenv: os.LookupEnv, Cwd: os.Getenv(helperCwd), Runner: osrun.New(),
	}
	cctx, err := commands.Context(env)
	code := 0
	if err == nil {
		_, err = commands.UpWithContext(context.Background(), cctx, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})
	}
	if err != nil {
		log.Error("FAILED: " + err.Error())
		code = 1
	}
	lines, _ := log.Snapshot()
	_ = os.WriteFile(os.Getenv(helperReport), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	return code
}

// helperEnvironment is this process's environment without any TREERIG_ variable
// or HOME of the person running the suite, plus extra: the helper must see
// the scratch treerig root and nothing of a real one.
func helperEnvironment(extra ...string) []string {
	var environment []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TREERIG_") && !strings.HasPrefix(entry, "HOME=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, extra...)
}

func TestConcurrentFirstUpsStartOneSharedServiceAcrossProcesses(t *testing.T) {
	w := newSharedWalk(t)
	goFile := filepath.Join(w.base, "go")

	type helper struct {
		cmd    *exec.Cmd
		report string
	}
	var helpers []helper
	var readyFiles []string
	for i, checkout := range []string{w.checkout, w.second} {
		report := filepath.Join(w.base, "report"+string(rune('a'+i)))
		ready := filepath.Join(w.base, "ready"+string(rune('a'+i)))
		readyFiles = append(readyFiles, ready)
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		home, _ := w.env.Getenv("HOME")
		router, _ := w.env.Getenv("TREERIG_ROUTER_PORT")
		cmd.Env = helperEnvironment(
			helperMode+"=1", helperCwd+"="+checkout, helperGo+"="+goFile, helperReady+"="+ready, helperReport+"="+report,
			"TREERIG_CONTROL_HOME="+w.controlHome, "HOME="+home, "TREERIG_ROUTER_PORT="+router)
		output := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		process := cmd.Process
		t.Cleanup(func() { _ = process.Kill() })
		helpers = append(helpers, helper{cmd: cmd, report: report})
	}
	// Release them only once both have reported that they are polling: a
	// started process may still be initialising, and the first `up` could
	// then finish before the second begins, which would test nothing.
	deadline := time.Now().Add(time.Minute)
	for _, ready := range readyFiles {
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a helper never reported ready (%s)", ready)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	writeFile(t, goFile, "go", 0o600)

	var all []string
	for i, h := range helpers {
		done := make(chan error, 1)
		go func() { done <- h.cmd.Wait() }()
		select {
		case err := <-done:
			data, _ := os.ReadFile(h.report)
			if err != nil {
				t.Fatalf("helper %d failed: %v\n%s", i, err, data)
			}
			all = append(all, string(data))
		case <-time.After(4 * time.Minute):
			t.Fatalf("helper %d never finished", i)
		}
	}

	if containers := w.sharedIDs(t, "ps", "-aq"); len(containers) != 1 {
		t.Fatalf("shared containers = %v, want exactly one for two concurrent first ups", containers)
	}
	started := strings.Count(strings.Join(all, "\n"), "Started shared service mailpit.")
	if started != 1 {
		t.Fatalf("%d processes started the shared service, want exactly one:\n%s", started, strings.Join(all, "\n----\n"))
	}
	for i, checkout := range []string{w.checkout, w.second} {
		env := w.secondEnv
		env.Cwd = checkout
		cctx := w.contextFor(t, &env)
		registration, err := cctx.Registry.Current(context.Background())
		if err != nil || registration == nil {
			t.Fatalf("checkout %d: %v, %v", i, registration, err)
		}
		api := w.owned(t, registration.WorktreeID, "ps", "-q", "--filter", "label=com.docker.compose.service=api")
		if len(api) != 1 {
			t.Fatalf("checkout %d api containers = %v", i, api)
		}
		if body, ok := reach(api[0], "http://mailpit:8025/"); !ok || body != "shared v1" {
			t.Errorf("checkout %d reaches mailpit as %q, %v", i, body, ok)
		}
	}
}
