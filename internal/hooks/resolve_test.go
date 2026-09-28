package hooks_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
)

// writeScript writes an executable file at path with contents, failing t on error.
func writeScript(t *testing.T, path string, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHook(t *testing.T) {
	t.Run("reports false for a stage that is not configured", func(t *testing.T) {
		_, ok, err := hooks.ResolveHook(config.Hooks{}, config.Prepare, "/root", nil)
		if ok || err != nil {
			t.Fatalf("ok = %v, err = %v", ok, err)
		}
	})

	t.Run("refuses an empty command the same way RunHook does, not as a run.StartError", func(t *testing.T) {
		empty := "   "
		_, ok, err := hooks.ResolveHook(config.Hooks{Prepare: &empty}, config.Prepare, "/root", nil)
		if !ok {
			t.Fatal("expected ok = true: the stage is configured")
		}
		var startErr *run.StartError
		if errors.As(err, &startErr) {
			t.Fatalf("expected a plain clierr, got a *run.StartError: %+v", startErr)
		}
		if err == nil || err.Error() != `Hook "prepare" is empty.` {
			t.Fatalf("err = %v", err)
		}
		var clierrErr *clierr.Error
		if !errors.As(err, &clierrErr) {
			t.Fatalf("err = %T, want *clierr.Error", err)
		}
	})

	t.Run("predicts 127 for a relative command with no file at its resolved path", func(t *testing.T) {
		root := registrytest.TempDir(t)
		command := "./verify"
		resolution, ok, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, nil)
		if !ok {
			t.Fatal("expected ok = true")
		}
		var startErr *run.StartError
		if !errors.As(err, &startErr) || startErr.ExitCode != 127 {
			t.Fatalf("err = %v", err)
		}
		if resolution.Command != filepath.Join(root, "verify") {
			t.Fatalf("resolution.Command = %q", resolution.Command)
		}
	})

	t.Run("predicts 126 for a relative command that is not executable", func(t *testing.T) {
		root := registrytest.TempDir(t)
		writeScript(t, filepath.Join(root, "verify"), "#!/bin/sh\n", 0o644)
		command := "./verify"
		_, _, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, nil)
		var startErr *run.StartError
		if !errors.As(err, &startErr) || startErr.ExitCode != 126 || startErr.Code != "EACCES" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("predicts 126, ENOEXEC, for an executable relative command with no #! line", func(t *testing.T) {
		root := registrytest.TempDir(t)
		writeScript(t, filepath.Join(root, "verify"), "not a script\n", 0o755)
		command := "./verify"
		_, _, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, nil)
		var startErr *run.StartError
		if !errors.As(err, &startErr) || startErr.ExitCode != 126 || startErr.Code != "ENOEXEC" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("resolves a relative command with a #! line and arguments", func(t *testing.T) {
		root := registrytest.TempDir(t)
		writeScript(t, filepath.Join(root, "verify"), "#!/bin/sh\n", 0o755)
		command := "./verify --strict now"
		resolution, ok, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, nil)
		if !ok || err != nil {
			t.Fatalf("ok = %v, err = %v", ok, err)
		}
		if resolution.Command != filepath.Join(root, "verify") {
			t.Fatalf("resolution.Command = %q", resolution.Command)
		}
		if len(resolution.Args) != 2 || resolution.Args[0] != "--strict" || resolution.Args[1] != "now" {
			t.Fatalf("resolution.Args = %v", resolution.Args)
		}
	})

	t.Run("resolves a bare command on the given env's own PATH", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		writeScript(t, filepath.Join(dir, "pg_dump"), "#!/bin/sh\n", 0o755)
		command := "pg_dump"
		resolution, ok, err := hooks.ResolveHook(config.Hooks{Backup: &command}, config.Backup, "/root", []string{"PATH=" + dir})
		if !ok || err != nil {
			t.Fatalf("ok = %v, err = %v", ok, err)
		}
		if resolution.Command != filepath.Join(dir, "pg_dump") {
			t.Fatalf("resolution.Command = %q", resolution.Command)
		}
	})

	t.Run("predicts 127 for a bare command on no PATH entry", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		command := "berth-definitely-missing-tool"
		_, ok, err := hooks.ResolveHook(config.Hooks{Restore: &command}, config.Restore, "/root", []string{"PATH=" + dir})
		if !ok {
			t.Fatal("expected ok = true")
		}
		var startErr *run.StartError
		if !errors.As(err, &startErr) || startErr.ExitCode != 127 {
			t.Fatalf("err = %v", err)
		}
	})

	// #105 round 1: root doubles as RunHook's own working directory, so a
	// relative "#!" interpreter must resolve against it, not PATH — exactly
	// what run.PredictStart documents and RunHook's own Dir: root gives a
	// real run.
	t.Run("resolves a relative #! interpreter against root, not PATH", func(t *testing.T) {
		root := registrytest.TempDir(t)
		// A native header, not another "#!" script: macOS refuses a script
		// as an interpreter, and this test is about path resolution.
		writeScript(t, filepath.Join(root, "myinterp"), "\x7fELF\x02\x01\x01\x00", 0o755)
		writeScript(t, filepath.Join(root, "verify"), "#!./myinterp\n", 0o755)
		command := "./verify"
		_, ok, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, []string{"PATH=/nonexistent"})
		if !ok || err != nil {
			t.Fatalf("ok = %v, err = %v", ok, err)
		}
	})

	// #105 round 1 (DevOps/SRE, Important): a CRLF shebang predicts the same
	// 127 a real run would hit — the kernel's own parser never strips the
	// trailing \r, so "/bin/sh\r" is what it actually tries to open.
	t.Run("predicts 127 with a CRLF hint for a shebang saved with CRLF line endings", func(t *testing.T) {
		root := registrytest.TempDir(t)
		writeScript(t, filepath.Join(root, "verify"), "#!/bin/sh\r\necho hi\r\n", 0o755)
		command := "./verify"
		_, _, err := hooks.ResolveHook(config.Hooks{Verify: &command}, config.Verify, root, nil)
		var startErr *run.StartError
		if !errors.As(err, &startErr) || startErr.ExitCode != 127 || startErr.Code != "ENOENT" {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(startErr.Detail, "CRLF") {
			t.Fatalf("detail = %q", startErr.Detail)
		}
	})
}
