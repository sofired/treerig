package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Load reads only a regular file of a sane size: a directory, a pipe and a
// file past MaxFileSize are refused without reading them.
func TestLoadRefusesWhatIsNotAConfigurationFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("a directory", func(t *testing.T) {
		path := filepath.Join(dir, "dir.toml")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a pipe with no writer is refused at once, not waited on", func(t *testing.T) {
		path := filepath.Join(dir, "pipe.toml")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("no fifo here: %v", err)
		}
		for name, read := range map[string]func() error{
			"Load":             func() error { _, err := Load(path); return err },
			"ReadRegularFile":  func() error { _, err := ReadRegularFile(path, MaxFileSize); return err },
			"CheckRegularFile": func() error { return CheckRegularFile(path, MaxFileSize) },
		} {
			done := make(chan error, 1)
			go func() { done <- read() }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
					t.Fatalf("%s: err = %v", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s is waiting for a writer to the pipe", name)
			}
		}
	})

	t.Run("a file past the limit", func(t *testing.T) {
		path := filepath.Join(dir, "big.toml")
		if err := os.WriteFile(path, []byte(strings.Repeat("# padding\n", MaxFileSize/8)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "is larger than") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a file followed through a symbolic link is read as usual", func(t *testing.T) {
		real := filepath.Join(dir, "real.toml")
		if err := os.WriteFile(real, []byte(minimalTOML), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.toml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); err != nil {
			t.Fatal(err)
		}
	})
}
