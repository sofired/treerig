//go:build integration

package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeRemoveRelocatesToAStandaloneBinary proves #7 through the real
// binary — never cli.Main in-process, which must never exec (see
// internal/cli's TestEnvBuilderRelocationWiring): `worktree remove`
// installs a content-addressed copy of the invoked binary under
// BERTH_HOME/bin, outside the checkout, before doing anything else, and
// hands the removal to that copy.
//
// No registered worktree is needed to prove this: relocation runs before
// Context even loads a config, so the simplest failing case — an empty
// checkout with no berth.toml — already exercises it. What matters is the
// outcome: a byte-identical copy lands outside the checkout, and the
// command still completes normally, presenting itself under the same name
// it was invoked as (brand.CLIName reads argv[0], which relocation must
// hand the installed copy unchanged).
func TestWorktreeRemoveRelocatesToAStandaloneBinary(t *testing.T) {
	binary := buildBinary(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controlHome := filepath.Join(base, "control")
	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "worktree", "remove", "--yes")
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), "BERTH_HOME="+controlHome)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	if got := exitErr.ExitCode(); got != 1 {
		t.Fatalf("exit code = %d, want 1\nstdout=%q\nstderr=%q", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "[berth] ERROR:") {
		t.Fatalf("stderr = %q, want the usual [berth] ERROR: prefix (argv[0] presentation must survive relocation)", stderr.String())
	}
	// Every fallback warning relocateAndReexec can print ends this way
	// (internal/commands/relocate.go); its presence would mean relocation
	// fell back to running in place rather than actually re-exec-ing, which
	// would make every check below true whether or not a real re-exec ever
	// happened (Install runs, and so does the rest of the assertion, either
	// way — see internal/commands's own relocation tests for the exec-path
	// coverage this integration test cannot safely exercise in-process).
	if strings.Contains(stderr.String(), "continuing with this binary") {
		t.Fatalf("stderr = %q, relocation fell back instead of re-exec-ing", stderr.String())
	}

	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("reading the built binary: %v", err)
	}
	sum := sha256.Sum256(content)
	binDir := filepath.Join(controlHome, "bin")
	installed := filepath.Join(binDir, hex.EncodeToString(sum[:]))

	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("the installed copy is missing at %s: %v", installed, err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed copy %s is not executable: mode %v", installed, info.Mode())
	}
	installedContent, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("reading the installed copy: %v", err)
	}
	if string(installedContent) != string(content) {
		t.Fatalf("installed copy's content differs from the binary it was relocated from")
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatalf("reading the standalone-binary store: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected exactly one entry in the standalone-binary store, got %v", names)
	}
}
