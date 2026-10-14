package files_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/stateformat"
)

func TestControlHomeFormat(t *testing.T) {
	t.Run("writes the format into the marker of a control home it creates", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(filepath.Join(home, files.ControlHomeMarker))
		if err != nil || string(contents) != files.ControlHomeFormat {
			t.Fatalf("marker = %q, %v", contents, err)
		}
	})

	t.Run("writes its own format over an existing but empty marker", func(t *testing.T) {
		// Empty carries nothing worth preserving, and this is exactly what
		// a concurrent securer of a brand new control home can observe
		// mid-write (see TestConcurrentControlHomeEstablishment): treated
		// the same as no marker at all, not refused.
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(marker); string(data) != files.ControlHomeFormat {
			t.Fatalf("marker = %q, want %q", data, files.ControlHomeFormat)
		}
	})

	t.Run("refuses an empty marker beside existing state, leaving both as they are", func(t *testing.T) {
		// A concurrent creator writes its marker in full before anything
		// else, so an empty marker next to real content is not one: it is
		// state this build did not write, and is not taken over.
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(filepath.Join(home, "repositories"), 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "is empty but the directory already holds") {
			t.Fatalf("got %v", err)
		}
		if data, _ := os.ReadFile(marker); len(data) != 0 {
			t.Fatalf("marker rewritten to %q", data)
		}
	})

	t.Run("refuses an existing control home whose marker names a format this build never wrote, leaving it as it is", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.WriteFile(marker, []byte("format 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "Refusing to use") || !strings.Contains(err.Error(), brand.Name+"'s own format") {
			t.Fatalf("got %v", err)
		}
		if data, _ := os.ReadFile(marker); string(data) != "format 1\n" {
			t.Fatalf("marker rewritten to %q", data)
		}
	})

	t.Run("refuses a marker planted as a symlink, leaving its target unchanged", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("not berth's to touch"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.Symlink(target, marker); err != nil {
			t.Fatal(err)
		}

		err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "Refusing symlinked or non-file managed path") {
			t.Fatalf("got %v", err)
		}
		data, readErr := os.ReadFile(target)
		if readErr != nil || string(data) != "not berth's to touch" {
			t.Fatalf("target = %q, %v", data, readErr)
		}
		if info, lstatErr := os.Lstat(marker); lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected the marker itself to remain a symlink: %v, %v", info, lstatErr)
		}
	})

	t.Run("refuses a marker symlinked to another file inside the same control home, not just one escaping it", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "sibling")
		if err := os.WriteFile(target, []byte("also not berth's to touch"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.Symlink(target, marker); err != nil {
			t.Fatal(err)
		}

		err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "Refusing symlinked or non-file managed path") {
			t.Fatalf("got %v", err)
		}
		data, readErr := os.ReadFile(target)
		if readErr != nil || string(data) != "also not berth's to touch" {
			t.Fatalf("target = %q, %v", data, readErr)
		}
	})

	t.Run("refuses a marker planted as a FIFO promptly, instead of hanging waiting for a reader or writer", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := unix.Mkfifo(marker, 0o600); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			done <- files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		}()

		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "Refusing symlinked or non-file managed path") {
				t.Fatalf("got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("establishControlHome hung on the FIFO instead of refusing it promptly")
		}

		info, err := os.Lstat(marker)
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("expected the marker to remain a FIFO: %v, %v", info, err)
		}
	})

	t.Run("refuses a control home swapped for a symlink to a directory between the checks and the open, leaving the target unmarked", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}

		// createSecureDirectories has already created and Lstat-checked home as
		// a real directory by the time this hook runs; swapping it for a
		// symlink here is the one window os.Open's own following would have
		// missed.
		restore := files.SetBeforeOpenControlHomeForTest(func(controlHome string) {
			if err := os.Remove(controlHome); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, controlHome); err != nil {
				t.Fatal(err)
			}
		})
		defer restore()

		err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "Refusing symlinked or non-directory managed path") {
			t.Fatalf("got %v", err)
		}

		exists, existsErr := files.PathExists(filepath.Join(elsewhere, files.ControlHomeMarker))
		if existsErr != nil {
			t.Fatal(existsErr)
		}
		if exists {
			t.Fatal("expected the swapped-to directory to get no marker")
		}
	})
}

// TestControlHomeFormatVersion covers the marker's version
// (docs/design/state.md): the marker this build writes is the one the version
// constant says, a newer one is refused with the upgrade instruction and left
// alone, and one that is not "format <n>" at all is foreign.
func TestControlHomeFormatVersion(t *testing.T) {
	t.Run("writes the line the format version constant names", func(t *testing.T) {
		if want := stateformat.FormatLine(stateformat.ControlHome) + "\n"; files.ControlHomeFormat != want {
			t.Fatalf("ControlHomeFormat = %q, want %q", files.ControlHomeFormat, want)
		}
	})

	t.Run("refuses a control home whose marker says a newer format, naming both versions and leaving it as it is", func(t *testing.T) {
		for _, marker := range []string{"format 3\n", "format 10\n"} {
			home := filepath.Join(t.TempDir(), "control")
			if err := os.MkdirAll(filepath.Join(home, "repositories"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, files.ControlHomeMarker)
			if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
			found := strings.TrimSpace(strings.TrimPrefix(marker, "format "))
			want := "Refusing to use the control home " + layout.ControlLayout(home).ControlHome + ": it is format " + found +
				", written by a newer berth, and this build reads format 2. Upgrade berth, then run this again."
			if err == nil || err.Error() != want {
				t.Fatalf("marker %q: got %v\nwant %s", marker, err, want)
			}
			if data, _ := os.ReadFile(path); string(data) != marker {
				t.Fatalf("marker rewritten to %q", data)
			}
		}
	})

	t.Run("treats a marker that is not exactly format and a number as foreign, not as a newer berth's", func(t *testing.T) {
		for _, marker := range []string{"format 3", "format 03\n", "format 3\n\n", "format\n", "3\n", "Format 3\n", "format 3 \n", "garbage\n", "format 1\n", "format 0\n"} {
			home := filepath.Join(t.TempDir(), "control")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, files.ControlHomeMarker)
			if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			err := files.SecureControlDirectories(layout.ControlLayout(home), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), brand.Name+"'s own format") || strings.Contains(err.Error(), "newer") {
				t.Fatalf("marker %q: got %v", marker, err)
			}
			if data, _ := os.ReadFile(path); string(data) != marker {
				t.Fatalf("marker %q rewritten to %q", marker, data)
			}
		}
	})
}

// TestAssertControlHomeFormat covers the read-only check `doctor` makes.
func TestAssertControlHomeFormat(t *testing.T) {
	home := func(t *testing.T, marker *string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if marker != nil {
			if err := os.WriteFile(filepath.Join(dir, files.ControlHomeMarker), []byte(*marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	text := func(s string) *string { return &s }

	t.Run("accepts this build's own marker", func(t *testing.T) {
		if err := files.AssertControlHomeFormat(home(t, text(files.ControlHomeFormat))); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("accepts a control home with no marker yet, writing nothing", func(t *testing.T) {
		dir := home(t, nil)
		if err := files.AssertControlHomeFormat(dir); err != nil {
			t.Fatal(err)
		}
		if exists, _ := files.PathExists(filepath.Join(dir, files.ControlHomeMarker)); exists {
			t.Fatal("a marker was written")
		}
	})

	t.Run("refuses a newer marker with the upgrade instruction", func(t *testing.T) {
		err := files.AssertControlHomeFormat(home(t, text("format 3\n")))
		if err == nil || !strings.Contains(err.Error(), "it is format 3, written by a newer berth, and this build reads format 2. Upgrade berth") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("refuses a foreign marker", func(t *testing.T) {
		err := files.AssertControlHomeFormat(home(t, text("hello\n")))
		if err == nil || !strings.Contains(err.Error(), "is not berth's own format") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("refuses a marker that is a symlink", func(t *testing.T) {
		dir := home(t, nil)
		if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, files.ControlHomeMarker)); err != nil {
			t.Fatal(err)
		}
		if err := files.AssertControlHomeFormat(dir); err == nil {
			t.Fatal("expected a refusal")
		}
	})
}
