package files_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// TestWriteNew pins WriteNew: it creates, never replaces, and leaves nothing behind.
func TestWriteNew(t *testing.T) {
	t.Run("creates the file with the exact mode and leaves nothing else", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		if err := files.WriteNew(dest, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != "hello\n" {
			t.Fatalf("contents = %q, %v; want hello", got, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("directory has %d entries, want only the file: %v", len(entries), entries)
		}
	})

	t.Run("sets the mode exactly even under a restrictive umask", func(t *testing.T) {
		// The umask is process-wide: this subtest must not run in parallel
		// with anything else (nothing in this package calls t.Parallel), and
		// restores the old one before it ends.
		previous := syscall.Umask(0o077)
		defer syscall.Umask(previous)
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		if err := files.WriteNew(dest, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v under umask 077, want exactly 0644", info.Mode().Perm())
		}
	})

	t.Run("refuses an existing file and leaves it untouched", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		if err := os.WriteFile(dest, []byte("mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := files.WriteNew(dest, []byte("new\n"), 0o644)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v, want fs.ErrExist", err)
		}
		if got, _ := os.ReadFile(dest); string(got) != "mine\n" {
			t.Errorf("contents = %q, want them untouched", got)
		}
		if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want the existing file's 0600 untouched", info.Mode().Perm())
		}
	})

	t.Run("refuses a dangling symlink and does not write through it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		target := filepath.Join(dir, "elsewhere")
		if err := os.Symlink(target, dest); err != nil {
			t.Fatal(err)
		}
		if err := files.WriteNew(dest, []byte("new\n"), 0o644); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v, want fs.ErrExist", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the symlink's target was created: %v", err)
		}
	})

	t.Run("names the destination, not a temp path, when its directory is missing", func(t *testing.T) {
		dest := filepath.Join(registrytest.TempDir(t), "missing", "berth.toml")
		err := files.WriteNew(dest, []byte("x"), 0o644)
		if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), dest) || strings.Contains(err.Error(), ".tmp") {
			t.Errorf("err = %v, want not-exist naming %s", err, dest)
		}
	})

	t.Run("removes the file it created when the write fails part way", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		boom := errors.New("disk full")
		err := files.CreateNewForTest(dest, 0o644, func(handle *os.File) error {
			if _, err := handle.Write([]byte("partial")); err != nil {
				t.Fatal(err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want it to wrap the write failure", err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the partial file was left behind: %v", err)
		}
		// With the debris gone a retry succeeds.
		if err := files.WriteNew(dest, []byte("whole"), 0o644); err != nil {
			t.Errorf("retry: %v", err)
		}
	})

	t.Run("lets exactly one of many racing writers win, with complete contents", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		dest := filepath.Join(dir, "berth.toml")
		const writers = 16
		results := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = files.WriteNew(dest, []byte(strings.Repeat(string(rune('a'+i))+"\n", 4096)), 0o644)
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, fs.ErrExist):
				t.Errorf("unexpected error: %v", err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d writers won, want exactly 1", wins)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
		if len(lines) != 4096 {
			t.Fatalf("file has %d lines, want 4096 (a whole winner's contents)", len(lines))
		}
		for _, line := range lines {
			if line != lines[0] {
				t.Fatalf("file mixes two writers' contents")
			}
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("directory has %d entries, want only the file: %v", len(entries), entries)
		}
	})
}
