//go:build integration

// Helpers shared by this package's real-daemon tests.
//
// Build those tests on the walk fixture (up_docker_integration_test.go),
// whose router is the test's own through commands.SharedRouter. A test that
// drives cli.Main directly has no way to rename the shared router's Compose
// project, so a verb that reaches the router (up, down, worktree remove)
// would act on a real `berth-router` if one were running on the machine.
package commands_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// dockerAvailable skips the test when no Docker daemon answers, unless
// BERTH_TEST_REQUIRE_DOCKER=1 makes its absence a failure instead.
// BERTH_TEST_SKIP_DOCKER=1 skips it even when a daemon answers, so CI can
// leave every Docker test to its own job.
func dockerAvailable(t *testing.T) {
	t.Helper()
	if os.Getenv("BERTH_TEST_SKIP_DOCKER") == "1" {
		t.Skip("Docker tests are skipped (BERTH_TEST_SKIP_DOCKER=1)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("BERTH_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (BERTH_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

// gitIn runs git with args in dir, isolated from any user or system Git
// config, and fails the test if it exits non-zero.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// dockerIDs runs docker with args and splits its output into whitespace-
// separated fields, the shape `docker ps -q` and similar id-listing calls
// return.
func dockerIDs(t *testing.T, args ...string) []string {
	t.Helper()
	output, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.Fields(string(output))
}
