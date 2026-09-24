package files_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

func TestControlHomeFormat(t *testing.T) {
	t.Run("writes the format into the marker of a control home it creates", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := files.SecureControlDirectories(layout.ControlLayout(home)); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(filepath.Join(home, files.ControlHomeMarker))
		if err != nil || string(contents) != files.ControlHomeFormat {
			t.Fatalf("marker = %q, %v", contents, err)
		}
		if legacy, _ := files.LegacyControlHome(home); legacy {
			t.Fatal("its own control home taken for the TypeScript build's")
		}
	})

	t.Run("tells a control home the TypeScript build left from one it wrote, and upgrades it on its first write", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		// The TypeScript build's marker: present and empty.
		if err := os.WriteFile(filepath.Join(home, files.ControlHomeMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if legacy, err := files.LegacyControlHome(home); err != nil || !legacy {
			t.Fatalf("legacy = %v, %v", legacy, err)
		}
		if err := files.SecureControlDirectories(layout.ControlLayout(home)); err != nil {
			t.Fatal(err)
		}
		if legacy, _ := files.LegacyControlHome(home); legacy {
			t.Fatal("still taken for the TypeScript build's after a write")
		}
	})

	t.Run("leaves a marker naming another format as it is", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(home, files.ControlHomeMarker)
		if err := os.WriteFile(marker, []byte("format 3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := files.SecureControlDirectories(layout.ControlLayout(home)); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(marker); string(data) != "format 3\n" {
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

		err := files.SecureControlDirectories(layout.ControlLayout(home))
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

		err := files.SecureControlDirectories(layout.ControlLayout(home))
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
			done <- files.SecureControlDirectories(layout.ControlLayout(home))
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

		err := files.SecureControlDirectories(layout.ControlLayout(home))
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

	t.Run("recognizes an install from before the marker by the tool's own directories", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		for _, name := range []string{"repositories", "locks"} {
			if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if legacy, err := files.LegacyControlHome(home); err != nil || !legacy {
			t.Fatalf("legacy = %v, %v", legacy, err)
		}
		missing := filepath.Join(t.TempDir(), "nothing-here")
		if legacy, err := files.LegacyControlHome(missing); err != nil || legacy {
			t.Fatalf("a missing home: legacy = %v, %v", legacy, err)
		}
	})
}
