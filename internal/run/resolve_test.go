package run_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
)

// writeExecutable writes contents to path with mode, failing t on error.
func writeExecutable(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveExecutable(t *testing.T) {
	t.Run("says a slash-qualified command that does not exist is missing, 127", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		_, err := run.ResolveExecutable(filepath.Join(dir, "missing"), nil)
		if err == nil || err.ExitCode != 127 || err.Code != "ENOENT" {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("says a slash-qualified command that is not executable is 126", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o644)
		_, err := run.ResolveExecutable(path, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("says a directory is 126, a regular-file problem not an executable-bit one", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := run.ResolveExecutable(sub, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
		if !strings.Contains(err.Detail, "is not a regular file") {
			t.Fatalf("detail = %q", err.Detail)
		}
	})

	t.Run("says a FIFO is 126 without ever opening it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "fifo")
		if err := unix.Mkfifo(path, 0o755); err != nil {
			t.Fatal(err)
		}
		done := make(chan *run.StartError, 1)
		go func() { _, err := run.ResolveExecutable(path, nil); done <- err }()
		select {
		case err := <-done:
			if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
				t.Fatalf("got %+v", err)
			}
			if !strings.Contains(err.Detail, "is not a regular file") {
				t.Fatalf("detail = %q", err.Detail)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ResolveExecutable hung on a FIFO")
		}
	})

	t.Run("finds a bare name on the env's own PATH, not this process's", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "tool")
		writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o755)
		resolved, err := run.ResolveExecutable("tool", []string{"PATH=" + dir})
		if err != nil {
			t.Fatalf("got %+v", err)
		}
		if resolved != path {
			t.Fatalf("resolved = %q, want %q", resolved, path)
		}
	})

	t.Run("says a bare name found on no PATH entry is missing, 127", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		_, err := run.ResolveExecutable("berth-definitely-missing-tool", []string{"PATH=" + dir})
		if err == nil || err.ExitCode != 127 || err.Code != "ENOENT" {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("says a bare name found on PATH only without its execute bit is EACCES, 126, naming the file", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "tool")
		writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o644)
		_, err := run.ResolveExecutable("tool", []string{"PATH=" + dir})
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
		if want := "`tool` could not be started: it is on PATH at " + path + ", but not executable (EACCES)."; err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})

	t.Run("keeps searching PATH past a non-executable match for one that runs", func(t *testing.T) {
		first, second := registrytest.TempDir(t), registrytest.TempDir(t)
		writeExecutable(t, filepath.Join(first, "tool"), []byte("#!/bin/sh\n"), 0o644)
		want := filepath.Join(second, "tool")
		writeExecutable(t, want, []byte("#!/bin/sh\n"), 0o755)
		resolved, err := run.ResolveExecutable("tool", []string{"PATH=" + first + string(filepath.ListSeparator) + second})
		if err != nil || resolved != want {
			t.Fatalf("resolved = %q, err = %+v, want %q", resolved, err, want)
		}
	})
}

func TestPathEnv(t *testing.T) {
	t.Run("reads PATH from env when given one", func(t *testing.T) {
		if got := run.PathEnv([]string{"HOME=/x", "PATH=/a:/b"}); got != "/a:/b" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("falls back to this process's own PATH when env is nil", func(t *testing.T) {
		if got := run.PathEnv(nil); got != os.Getenv("PATH") {
			t.Fatalf("got %q, want %q", got, os.Getenv("PATH"))
		}
	})

	t.Run("is empty when env is given but has no PATH entry", func(t *testing.T) {
		if got := run.PathEnv([]string{"HOME=/x"}); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestPredictStart(t *testing.T) {
	t.Run("predicts 127 for a missing file", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		_, err := run.PredictStart(filepath.Join(dir, "missing"), dir, nil)
		if err == nil || err.ExitCode != 127 {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("predicts 126 for a file that is not executable", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o644)
		_, err := run.PredictStart(path, dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("predicts 126 for a directory, never for a FIFO it would have to open", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		fifo := filepath.Join(dir, "fifo")
		if err := unix.Mkfifo(fifo, 0o755); err != nil {
			t.Fatal(err)
		}
		done := make(chan *run.StartError, 1)
		go func() { _, err := run.PredictStart(fifo, dir, nil); done <- err }()
		select {
		case err := <-done:
			if err == nil || err.ExitCode != 126 || !strings.Contains(err.Detail, "is not a regular file") {
				t.Fatalf("got %+v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("PredictStart hung on a FIFO")
		}
	})

	t.Run("predicts 126, ENOEXEC, for an executable file with no #! line and no native header", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("just some text\n"), 0o755)
		_, err := run.PredictStart(path, dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "ENOEXEC" {
			t.Fatalf("got %+v", err)
		}
		if err.Detail != "is not a valid executable (ENOEXEC) — a script needs a #! line." {
			t.Fatalf("detail = %q", err.Detail)
		}
	})

	t.Run("succeeds for an executable file with a #! line", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/bin/sh\necho hi\n"), 0o755)
		resolved, err := run.PredictStart(path, dir, nil)
		if err != nil {
			t.Fatalf("got %+v", err)
		}
		if resolved != path {
			t.Fatalf("resolved = %q, want %q", resolved, path)
		}
	})

	t.Run("succeeds for a native ELF header with no #! line", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
		if _, err := run.PredictStart(path, dir, nil); err != nil {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("treats an execute-only file as runnable: its format cannot be checked, but that is not ENOEXEC", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o111)
		resolved, err := run.PredictStart(path, dir, nil)
		if err != nil {
			t.Fatalf("got %+v", err)
		}
		if resolved != path {
			t.Fatalf("resolved = %q, want %q", resolved, path)
		}
	})

	t.Run("succeeds for #!/usr/bin/env bash: the interpreter is /usr/bin/env, bash is env's own problem", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/usr/bin/env bash\necho hi\n"), 0o755)
		if _, err := run.PredictStart(path, dir, nil); err != nil {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("predicts 127 for a shebang naming an interpreter that does not exist", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		writeExecutable(t, path, []byte("#!/no/such/interpreter\n"), 0o755)
		_, err := run.PredictStart(path, dir, nil)
		if err == nil || err.ExitCode != 127 || err.Code != "ENOENT" {
			t.Fatalf("got %+v", err)
		}
		if strings.Contains(err.Detail, "CRLF") {
			t.Fatalf("expected no CRLF hint, detail = %q", err.Detail)
		}
		if !strings.Contains(err.Detail, "/no/such/interpreter") {
			t.Fatalf("detail = %q", err.Detail)
		}
	})

	t.Run("predicts 127 with a CRLF hint for a shebang saved with CRLF line endings", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "hook")
		// "/bin/sh\r" is what the kernel's own shebang parser sees as the
		// interpreter name (it splits on space/tab, never '\r'), and no
		// file by that literal name exists.
		writeExecutable(t, path, []byte("#!/bin/sh\r\necho hi\r\n"), 0o755)
		_, err := run.PredictStart(path, dir, nil)
		if err == nil || err.ExitCode != 127 || err.Code != "ENOENT" {
			t.Fatalf("got %+v", err)
		}
		if !strings.Contains(err.Detail, "CRLF") {
			t.Fatalf("expected a CRLF hint, detail = %q", err.Detail)
		}
		// The displayed interpreter name is the clean one, not a raw \r.
		if !strings.Contains(err.Detail, "`/bin/sh`") || strings.ContainsRune(err.Detail, '\r') {
			t.Fatalf("detail = %q", err.Detail)
		}
	})

	t.Run("resolves a relative interpreter against dir, never PATH", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		interp := filepath.Join(dir, "myinterp")
		// A native header, not another "#!" script: this test is about
		// relative-path resolution, not interpreter recursion (see
		// TestPredictStartInterpreterFormat for that).
		writeExecutable(t, interp, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
		script := filepath.Join(dir, "hook")
		writeExecutable(t, script, []byte("#!./myinterp\n"), 0o755)
		if _, err := run.PredictStart(script, dir, []string{"PATH=/nonexistent"}); err != nil {
			t.Fatalf("got %+v", err)
		}
	})
}

// TestPredictStartInterpreterFormat is #105 round 2 (DevOps/SRE, Important):
// after an interpreter is found and executable, exec(2) still has to run
// it, and its own format matters — a native binary always works, but
// whether a script may itself be a script's interpreter, and how deep that
// chain may go, is platform-specific (see resolve_linux.go and
// resolve_darwin.go's own shebangMaxDepth).
func TestPredictStartInterpreterFormat(t *testing.T) {
	t.Run("classifies a native interpreter's own header, regardless of platform", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		interp := filepath.Join(dir, "interp")
		writeExecutable(t, interp, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}, 0o755) // 64-bit Mach-O
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!./interp\n"), 0o755)
		if _, err := run.PredictStart(hook, dir, nil); err != nil {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("a script-as-interpreter: runnable on Linux, refused as ENOEXEC on Darwin", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		nativeInterp := filepath.Join(dir, "real-interp")
		writeExecutable(t, nativeInterp, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
		scriptInterp := filepath.Join(dir, "shim")
		writeExecutable(t, scriptInterp, []byte("#!./real-interp\n"), 0o755)
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!./shim\n"), 0o755)

		_, err := run.PredictStart(hook, dir, nil)
		switch runtime.GOOS {
		case "darwin":
			if err == nil || err.ExitCode != 126 || err.Code != "ENOEXEC" {
				t.Fatalf("got %+v", err)
			}
			if !strings.Contains(err.Detail, "shim") || !strings.Contains(err.Detail, "itself a script") {
				t.Fatalf("detail = %q", err.Detail)
			}
		case "linux":
			if err != nil {
				t.Fatalf("got %+v", err)
			}
		default:
			t.Skipf("no shebangMaxDepth expectation recorded for GOOS=%s", runtime.GOOS)
		}
	})

	t.Run("too many levels of #! interpreters is refused on Linux", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("shebangMaxDepth is Linux-specific behaviour; darwin refuses at the first script-as-interpreter instead")
		}
		dir := registrytest.TempDir(t)
		// hook -> h1 -> h2 -> h3 -> h4 -> h5 -> h6: six interpreter
		// indirections past the hook's own script, one past
		// shebangMaxDepth (5).
		names := []string{"hook", "h1", "h2", "h3", "h4", "h5", "h6"}
		for i, name := range names {
			path := filepath.Join(dir, name)
			if i == len(names)-1 {
				writeExecutable(t, path, []byte("#!/bin/sh\n"), 0o755)
				continue
			}
			writeExecutable(t, path, []byte("#!./"+names[i+1]+"\n"), 0o755)
		}
		_, err := run.PredictStart(filepath.Join(dir, "hook"), dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "ELOOP" {
			t.Fatalf("got %+v", err)
		}
		if !strings.Contains(err.Detail, "too many levels deep") {
			t.Fatalf("detail = %q", err.Detail)
		}
	})

	t.Run("a native binary one level past the limit is refused on Linux, as the kernel stops before loading it", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("execMaxDepth is Linux-specific behaviour")
		}
		dir := registrytest.TempDir(t)
		// hook -> h1 -> h2 -> h3 -> h4 -> h5 -> h6, a native ELF at
		// depth 6: exec_binprm returns ELOOP before it looks at h6.
		names := []string{"hook", "h1", "h2", "h3", "h4", "h5", "h6"}
		for i, name := range names {
			path := filepath.Join(dir, name)
			if i == len(names)-1 {
				writeExecutable(t, path, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
				continue
			}
			writeExecutable(t, path, []byte("#!./"+names[i+1]+"\n"), 0o755)
		}
		_, err := run.PredictStart(filepath.Join(dir, "hook"), dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "ELOOP" {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("a chain up to the limit still succeeds on Linux", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("shebangMaxDepth is Linux-specific behaviour")
		}
		dir := registrytest.TempDir(t)
		// hook -> h1 -> h2 -> h3 -> h4 -> h5, a native ELF: five
		// interpreter indirections, exactly shebangMaxDepth.
		names := []string{"hook", "h1", "h2", "h3", "h4", "h5"}
		for i, name := range names {
			path := filepath.Join(dir, name)
			if i == len(names)-1 {
				writeExecutable(t, path, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
				continue
			}
			writeExecutable(t, path, []byte("#!./"+names[i+1]+"\n"), 0o755)
		}
		if _, err := run.PredictStart(filepath.Join(dir, "hook"), dir, nil); err != nil {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("an interpreter that is neither a script nor native is its own ENOEXEC", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		interp := filepath.Join(dir, "garbage-interp")
		writeExecutable(t, interp, []byte("not a script, not a binary\n"), 0o755)
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!./garbage-interp\n"), 0o755)
		_, err := run.PredictStart(hook, dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "ENOEXEC" {
			t.Fatalf("got %+v", err)
		}
		if !strings.Contains(err.Detail, "garbage-interp") {
			t.Fatalf("detail = %q, want it to name the interpreter, not the hook", err.Detail)
		}
		if want := "could not be started: it has a #! interpreter `"; !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to read %q", err.Error(), want)
		}
	})

	// The interpreter is resolved through the very same Stat-then-Access
	// check as the hook's own file (see ResolveExecutable's own doc
	// comment): a FIFO interpreter is refused, never opened, and this must
	// not hang.
	t.Run("a FIFO interpreter is refused without ever being opened", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		fifo := filepath.Join(dir, "fifo-interp")
		if err := unix.Mkfifo(fifo, 0o755); err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!./fifo-interp\n"), 0o755)
		done := make(chan *run.StartError, 1)
		go func() { _, err := run.PredictStart(hook, dir, nil); done <- err }()
		select {
		case err := <-done:
			if err == nil || err.ExitCode != 126 || !strings.Contains(err.Detail, "is not executable") {
				t.Fatalf("got %+v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("PredictStart hung on a FIFO interpreter")
		}
	})
}

// lockedDir makes a directory holding one executable file, then removes
// search permission from the directory, so stat on the file fails with
// EACCES. Skips when running as root, which search permission never stops.
func lockedDir(t *testing.T) (dir, file string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory search permission")
	}
	dir = filepath.Join(registrytest.TempDir(t), "locked")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "tool")
	writeExecutable(t, file, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755)
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir, file
}

// A stat failure that isn't "does not exist" is 126 with the errno exec(2)
// would report; only a missing file is 127.
func TestPredictStartStatFailures(t *testing.T) {
	t.Run("a command under an unsearchable directory is EACCES, not missing", func(t *testing.T) {
		_, file := lockedDir(t)
		_, err := run.PredictStart(file, "", nil)
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
		if want := "could not be started: it cannot be reached: a directory on its path is not searchable (EACCES)."; !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("error = %q, want it to end %q", err.Error(), want)
		}
	})

	t.Run("a command whose path runs through a regular file is ENOTDIR, 126", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		file := filepath.Join(dir, "plain")
		writeExecutable(t, file, []byte("data\n"), 0o644)
		_, err := run.PredictStart(filepath.Join(file, "child"), dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != syscall.ENOTDIR.Error() {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("an interpreter under an unsearchable directory is EACCES, naming the interpreter", func(t *testing.T) {
		_, interp := lockedDir(t)
		dir := registrytest.TempDir(t)
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!"+interp+"\n"), 0o755)
		_, err := run.PredictStart(hook, dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != "EACCES" {
			t.Fatalf("got %+v", err)
		}
		if want := "has a #! interpreter `" + interp + "` that cannot be reached: a directory on its path is not searchable (EACCES)."; !strings.Contains(err.Detail, want) {
			t.Fatalf("detail = %q, want %q", err.Detail, want)
		}
	})

	t.Run("an interpreter whose path runs through a regular file is ENOTDIR, 126", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		writeExecutable(t, filepath.Join(dir, "plain"), []byte("data\n"), 0o644)
		hook := filepath.Join(dir, "hook")
		writeExecutable(t, hook, []byte("#!./plain/child\n"), 0o755)
		_, err := run.PredictStart(hook, dir, nil)
		if err == nil || err.ExitCode != 126 || err.Code != syscall.ENOTDIR.Error() {
			t.Fatalf("got %+v", err)
		}
	})

	t.Run("a #! line naming no interpreter is ENOEXEC", func(t *testing.T) {
		for _, line := range []string{"#!\n", "#!  \t\n", "#!"} {
			dir := registrytest.TempDir(t)
			hook := filepath.Join(dir, "hook")
			writeExecutable(t, hook, []byte(line), 0o755)
			_, err := run.PredictStart(hook, dir, nil)
			if err == nil || err.ExitCode != 126 || err.Code != "ENOEXEC" {
				t.Fatalf("%q: got %+v", line, err)
			}
			if !strings.Contains(err.Detail, "names no interpreter") {
				t.Fatalf("%q: detail = %q", line, err.Detail)
			}
		}
	})
}
