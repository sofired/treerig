//go:build integration

package cli_test

// `worktree remove`'s teardown gate through real processes (#146,
// docs/design/teardown.md): it refuses without a pin, runs the pinned build
// and not the one that was invoked, keeps the caller's argv[0], maps a stored
// binary that cannot be executed, and treats --dev-teardown as a flag and
// nothing else. No Docker is involved: every case ends at the gate or at the
// first thing after it that needs a treerig.toml.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinWithEnv pins binary as the teardown binary (a file pin, labelled label)
// for the treerig root that env names, running it from cwd, and fails t when
// that does not succeed. It is how every suite that removes through a binary
// pins first (docs/design/teardown.md, "Order of work", step 6).
func pinWithEnv(t *testing.T, binary, cwd string, env []string, label string) {
	t.Helper()
	cmd := exec.Command(binary, "teardown", "pin", "--file", binary, "--sha256", fileSHA256(t, binary), "--release", label, "--yes")
	cmd.Dir = cwd
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pinning %s: %v\n%s", binary, err, output)
	}
}

func TestTeardownGate(t *testing.T) {
	pinned := buildVersioned(t, "pinned-test")
	invoked := buildVersioned(t, "invoked-9.9.9")
	released := buildVersioned(t, "0.1.0-rc.3")
	pinnedDigest := fileSHA256(t, pinned)

	// pinnedRoot is a scratch root with `pinned` pinned in it, and the path
	// it is stored at.
	pinnedRoot := func(t *testing.T) (dir, stored string) {
		t.Helper()
		dir = scratchTeardownRoot(t)
		result := runTreerig(t, pinned, dir, []string{"teardown", "pin", "--file", pinned, "--sha256", pinnedDigest, "--release", "pinned-label", "--yes"}, treerigOptions{})
		if result.Code != 0 {
			t.Fatalf("pin = %+v", result)
		}
		return dir, filepath.Join(dir, "control-home", "control-home", "teardown", pinnedDigest)
	}

	t.Run("refuses without a pin, with the exact text, and writes nothing", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		want := "[treerig] ERROR: Refusing to remove this worktree: this is a development build (invoked-9.9.9), which may carry a branch's code, and no teardown binary is pinned. A development build removes only with a pinned one (a release build needs none).\n" +
			"  Pin the latest release: `treerig teardown pin --latest`\n" +
			"  or a treerig you trust: `treerig teardown pin --file <a treerig you trust> --sha256 <its sha256> --release <a label>`\n" +
			"  (`treerig teardown pin --help` explains both). To remove with this build's own code while you work on removal itself, add `--dev-teardown`. Nothing was removed.\n"
		for name, opts := range map[string]treerigOptions{
			"plain": {},
			// The escape hatch is a flag and nothing else: no variable turns it on.
			"with variables that might name the escape hatch": {env: map[string]string{"TREERIG_DEV_TEARDOWN": "1", "TREERIG_DEVTEARDOWN": "1", "DEV_TEARDOWN": "1"}},
		} {
			result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, opts)
			if result.Code != 1 || result.Stdout != "" || result.Stderr != want {
				t.Fatalf("%s: remove = %+v", name, result)
			}
		}
		for _, name := range []string{"bin", "teardown"} {
			if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", name)); !os.IsNotExist(err) {
				t.Errorf("%s: %v; a refusal must write nothing", name, err)
			}
		}
		named := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes", "feature-login"}, treerigOptions{})
		if named.Code != 1 || !strings.HasPrefix(named.Stderr, "[treerig] ERROR: Refusing to remove the worktree feature-login: this is a development build") {
			t.Fatalf("named remove = %+v", named)
		}
	})

	t.Run("with no pin, a release build removes with itself, relocated through bin/, and says nothing about a pin", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		result := runTreerig(t, released, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{timeout: 60 * time.Second})
		// Past the gate, the removal needs no treerig.toml, and stops because
		// the scratch directory is no Git worktree and no worktree was named.
		if result.Code != 1 || result.Stdout != "" || !strings.HasPrefix(result.Stderr, "[treerig] ERROR: There is no worktree to remove here: ") ||
			strings.Contains(result.Stderr, "Refusing") || strings.Contains(result.Stderr, "WARNING") || strings.Contains(result.Stderr, "pinned") {
			t.Fatalf("remove = %+v", result)
		}
		digest := fileSHA256(t, released)
		if got := fileSHA256(t, filepath.Join(dir, "control-home", "control-home", "bin", digest)); got != digest {
			t.Fatalf("relocated copy sha256 = %s", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown")); !os.IsNotExist(err) {
			t.Fatalf("teardown/: %v; an unpinned release writes no pin store", err)
		}
	})

	t.Run("a pin wins for a release build, and --dev-teardown bypasses it with the warning", func(t *testing.T) {
		dir, stored := pinnedRoot(t)
		result := runTreerig(t, released, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		want := "[treerig] Running the pinned teardown binary " + stored + " (release pinned-label) to remove this worktree.\n" +
			"[treerig] Running as the pinned teardown binary (version pinned-test).\n"
		if result.Code != 1 || !strings.HasPrefix(result.Stderr, want) {
			t.Fatalf("remove = %+v\nwant stderr to start with\n%s", result, want)
		}
		bypass := runTreerig(t, released, dir, []string{"worktree", "remove", "--yes", "--dev-teardown"}, treerigOptions{})
		warning := "[treerig] WARNING: Removing with this build's own code (version 0.1.0-rc.3, sha256 " + fileSHA256(t, released) + ")"
		if bypass.Code != 1 || !strings.HasPrefix(bypass.Stderr, warning) || strings.Contains(bypass.Stderr, "pinned teardown binary (version") {
			t.Fatalf("--dev-teardown on a release build = %+v", bypass)
		}
		// A broken pin still refuses a release build.
		if err := os.WriteFile(stored, []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		broken := runTreerig(t, released, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		if broken.Code != 1 || !strings.HasPrefix(broken.Stderr, "[treerig] ERROR: Refusing to remove this worktree: the pinned teardown binary "+stored+" has sha256 ") {
			t.Fatalf("broken pin = %+v", broken)
		}
	})

	t.Run("runs the pinned build, not the one that was invoked, under the caller's name", func(t *testing.T) {
		dir, stored := pinnedRoot(t)
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{argv0: "trig"})
		want := "[trig] Running the pinned teardown binary " + stored + " (release pinned-label) to remove this worktree.\n" +
			"[trig] Running as the pinned teardown binary (version pinned-test).\n"
		if result.Code != 1 || result.Stdout != "" || !strings.HasPrefix(result.Stderr, want) {
			t.Fatalf("remove = %+v\nwant stderr to start with\n%s", result, want)
		}
		// What follows the gate is the pinned build's own removal, which needs
		// a treerig.toml this directory does not have.
		rest := strings.TrimPrefix(result.Stderr, want)
		if !strings.HasPrefix(rest, "[trig] ERROR: ") || strings.Contains(result.Stderr, "invoked-9.9.9") || strings.Contains(result.Stderr, "Refusing") {
			t.Fatalf("after the gate: %q", rest)
		}
		// Neither a relocated copy nor anything else was written to bin/.
		if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "bin")); !os.IsNotExist(err) {
			t.Fatalf("bin/: %v; the pinned path must not copy the invoked build", err)
		}
	})

	t.Run("the pinned build's line proves its code ran: only it knows its version", func(t *testing.T) {
		dir, _ := pinnedRoot(t)
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		if !strings.Contains(result.Stderr, "(version pinned-test).") || strings.Contains(result.Stderr, "invoked-9.9.9") {
			t.Fatalf("stderr = %q", result.Stderr)
		}
	})

	t.Run("refuses a stored binary whose bytes changed", func(t *testing.T) {
		dir, stored := pinnedRoot(t)
		if err := os.WriteFile(stored, []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		wantStart := "[treerig] ERROR: Refusing to remove this worktree: the pinned teardown binary " + stored + " has sha256 "
		if result.Code != 1 || result.Stdout != "" || !strings.HasPrefix(result.Stderr, wantStart) || !strings.HasSuffix(result.Stderr, ", not the pinned "+pinnedDigest+". Pin it again with `treerig teardown pin --file "+pinned+" --sha256 "+pinnedDigest+" --release pinned-label`. Nothing was removed.\n") {
			t.Fatalf("remove = %+v", result)
		}
	})

	t.Run("refuses, naming the control home, a stored binary that cannot be executed", func(t *testing.T) {
		dir, stored := pinnedRoot(t)
		// The bytes still hash right; only the mode keeps the exec from happening, as a noexec mount would.
		if err := os.Chmod(stored, 0o600); err != nil {
			t.Fatal(err)
		}
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		wantStart := "[treerig] Running the pinned teardown binary " + stored + " (release pinned-label) to remove this worktree.\n" +
			"[treerig] ERROR: Refusing to remove this worktree: could not run the pinned teardown binary " + stored + " (permission denied). " +
			"If the filesystem holding the control home (" + filepath.Join(dir, "control-home", "control-home") + ") cannot run programs, remount it with exec, or move the control home with TREERIG_CONTROL_HOME. Nothing was removed.\n"
		if result.Code != 1 || result.Stdout != "" || result.Stderr != wantStart {
			t.Fatalf("remove = %+v\nwant stderr\n%s", result, wantStart)
		}
	})

	t.Run("a pinned binary run directly proceeds only when the pin names it, and refuses --dev-teardown", func(t *testing.T) {
		dir, stored := pinnedRoot(t)
		direct := runTreerig(t, stored, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{argv0: "treerig"})
		if direct.Code != 1 || !strings.HasPrefix(direct.Stderr, "[treerig] Running as the pinned teardown binary (version pinned-test).\n") {
			t.Fatalf("direct = %+v", direct)
		}
		dev := runTreerig(t, stored, dir, []string{"worktree", "remove", "--yes", "--dev-teardown"}, treerigOptions{argv0: "treerig"})
		if dev.Code != 1 || dev.Stdout != "" || dev.Stderr != "[treerig] ERROR: Refusing --dev-teardown: this is a pinned teardown binary. Nothing was removed.\n" {
			t.Fatalf("--dev-teardown = %+v", dev)
		}
		// With the pin gone, the same file is no longer a pinned binary.
		if err := os.Remove(filepath.Join(dir, "control-home", "control-home", "teardown", "pin.json")); err != nil {
			t.Fatal(err)
		}
		orphan := runTreerig(t, stored, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{argv0: "treerig"})
		if orphan.Code != 1 || !strings.Contains(orphan.Stderr, "this teardown binary "+stored+" is not the one the pin names") {
			t.Fatalf("orphan = %+v", orphan)
		}
	})

	t.Run("--dev-teardown warns once, relocates this build through bin/, and needs no pin", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		digest := fileSHA256(t, invoked)
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes", "--dev-teardown"}, treerigOptions{argv0: "trig"})
		warning := "[trig] WARNING: Removing with this build's own code (version invoked-9.9.9, sha256 " + digest + "), not a pinned teardown binary: code from this build runs during the removal.\n"
		if result.Code != 1 || result.Stdout != "" || !strings.HasPrefix(result.Stderr, warning) {
			t.Fatalf("remove = %+v\nwant stderr to start with\n%s", result, warning)
		}
		if strings.Count(result.Stderr, "WARNING") != 1 || strings.Contains(result.Stderr, "continuing with this binary") {
			t.Fatalf("stderr = %q; want one warning, from the parent, and a real relocation", result.Stderr)
		}
		if got := fileSHA256(t, filepath.Join(dir, "control-home", "control-home", "bin", digest)); got != digest {
			t.Fatalf("relocated copy sha256 = %s", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown")); !os.IsNotExist(err) {
			t.Fatalf("teardown/: %v; --dev-teardown must not touch the pin", err)
		}
		// Every run warns; the flag is not remembered.
		again := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes", "--dev-teardown"}, treerigOptions{})
		if !strings.Contains(again.Stderr, "WARNING: Removing with this build's own code") {
			t.Fatalf("second run = %+v", again)
		}
		plain := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{})
		if !strings.Contains(plain.Stderr, "Refusing to remove this worktree: this is a development build") {
			t.Fatalf("a plain run after --dev-teardown = %+v; the flag must not be remembered", plain)
		}
	})

	t.Run("refuses a control home that an environment file pointed into a checkout, where the branch planted its own pin", func(t *testing.T) {
		// A branch commits a pin and a binary named by its sha256, and a .envrc
		// that sets TREERIG_CONTROL_HOME to them. The pin is valid and intact;
		// where it lives is what the gate refuses.
		dir := scratchTeardownRoot(t)
		checkout := filepath.Join(dir, "checkout")
		if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		planted := filepath.Join(checkout, ".pin")
		opts := treerigOptions{env: map[string]string{"TREERIG_CONTROL_HOME": planted, "HOME": filepath.Join(dir, "home")}}
		if err := os.MkdirAll(opts.env["HOME"], 0o700); err != nil {
			t.Fatal(err)
		}
		if result := runTreerig(t, pinned, checkout, []string{"teardown", "pin", "--file", pinned, "--sha256", pinnedDigest, "--release", "planted", "--yes"}, opts); result.Code != 0 {
			t.Fatalf("pin = %+v", result)
		}
		result := runTreerig(t, invoked, checkout, []string{"worktree", "remove", "--yes"}, opts)
		want := "[treerig] ERROR: Refusing to remove this worktree: " + planted + "/ is inside the git work tree " + checkout + "/" +
			", where a branch could plant its own teardown pin. Keep the control home outside every checkout: move TREERIG_HOME (or TREERIG_CONTROL_HOME, if you set it) to a directory that is not inside a git work tree. Nothing was removed.\n"
		if result.Code != 1 || result.Stdout != "" || result.Stderr != want {
			t.Fatalf("remove = %+v\nwant stderr\n%s", result, want)
		}
		status := runTreerig(t, invoked, checkout, []string{"teardown", "status"}, opts)
		if !strings.Contains(status.Stdout, "worktree remove: would refuse") || !strings.Contains(status.Stderr, "/ is inside the git work tree "+checkout+"/") {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("a control home reached through a symlink still works, and the pinned child recognises itself instead of exec-ing in a loop", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		// The control home itself cannot be a symlink, but a directory above it can.
		parentReal := filepath.Join(dir, "real")
		parentLink := filepath.Join(dir, "linked")
		if err := os.MkdirAll(parentReal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(parentReal, parentLink); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parentLink, "control")
		opts := treerigOptions{env: map[string]string{"TREERIG_CONTROL_HOME": link}, timeout: 60 * time.Second}
		if result := runTreerig(t, pinned, dir, []string{"teardown", "pin", "--file", pinned, "--sha256", pinnedDigest, "--release", "linked", "--yes"}, opts); result.Code != 0 {
			t.Fatalf("pin = %+v", result)
		}
		result := runTreerig(t, invoked, dir, []string{"worktree", "remove", "--yes"}, opts)
		parent := "[treerig] Running the pinned teardown binary " + filepath.Join(link, "teardown", pinnedDigest) + " (release linked) to remove this worktree.\n"
		child := "[treerig] Running as the pinned teardown binary (version pinned-test).\n"
		if result.Code != 1 || !strings.HasPrefix(result.Stderr, parent+child) {
			t.Fatalf("remove = %+v\nwant stderr to start with\n%s%s", result, parent, child)
		}
	})

	t.Run("refuses when the checkout's environment file also sets HOME, so that a home taken from the environment would exempt the checkout", func(t *testing.T) {
		// HOME=$PWD, or HOME=$PWD/sub, and TREERIG_CONTROL_HOME under it, with a
		// valid pin planted there. The exemption for a dotfiles repository must
		// come from the user database, never from HOME.
		for name, homeUnder := range map[string]string{"HOME is the checkout": "", "HOME is a directory in it": "sub"} {
			dir := scratchTeardownRoot(t)
			checkout := filepath.Join(dir, "checkout")
			home := filepath.Join(checkout, homeUnder)
			if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			planted := filepath.Join(home, ".c")
			opts := treerigOptions{env: map[string]string{"TREERIG_CONTROL_HOME": planted, "HOME": home}}
			if result := runTreerig(t, pinned, checkout, []string{"teardown", "pin", "--file", pinned, "--sha256", pinnedDigest, "--release", "planted", "--yes"}, opts); result.Code != 0 {
				t.Fatalf("%s: pin = %+v", name, result)
			}
			result := runTreerig(t, released, checkout, []string{"worktree", "remove", "--yes"}, opts) // a release build refuses a planted pin too
			want := "[treerig] ERROR: Refusing to remove this worktree: " + planted + "/ is inside the git work tree " + checkout + "/" +
				", where a branch could plant its own teardown pin. Keep the control home outside every checkout: move TREERIG_HOME (or TREERIG_CONTROL_HOME, if you set it) to a directory that is not inside a git work tree. Nothing was removed.\n"
			if result.Code != 1 || result.Stdout != "" || result.Stderr != want {
				t.Fatalf("%s: remove = %+v\nwant stderr\n%s", name, result, want)
			}
		}
	})
}
