package teardown_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/teardown"
)

// fileMode is path's permission bits.
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestSave(t *testing.T) {
	t.Run("writes the binary and the pin owner-only, the binary named by its sha256", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "binary one"})
		saved, err := teardown.Save(l, home, []byte("binary one"), pin, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(l.TeardownDir, binstore.Hash([]byte("binary one")))
		if saved.BinaryPath != want || teardown.BinaryPath(l, binstore.Hash([]byte("binary one"))) != want {
			t.Errorf("BinaryPath = %q, want %q", saved.BinaryPath, want)
		}
		for path, mode := range map[string]os.FileMode{l.TeardownDir: 0o700, l.PinPath: 0o600, want: 0o700} {
			if got := fileMode(t, path); got != mode {
				t.Errorf("%s mode = %o, want %o", path, got, mode)
			}
		}
		if got, _ := os.ReadFile(want); string(got) != "binary one" {
			t.Errorf("binary = %q", got)
		}
		if saved.SweepErr != nil || len(saved.Removed) != 0 {
			t.Errorf("sweep = %v, %v", saved.Removed, saved.SweepErr)
		}
	})

	t.Run("tightens a loose teardown directory and a loose binary it reuses", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		binary := teardown.BinaryPath(l, binstore.Hash([]byte("b")))
		if err := os.Chmod(l.TeardownDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(binary, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		if fileMode(t, l.TeardownDir) != 0o700 || fileMode(t, binary) != 0o700 {
			t.Errorf("modes = %o, %o", fileMode(t, l.TeardownDir), fileMode(t, binary))
		}
	})

	t.Run("writes nothing when the pin does not name the binary", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "something else"})
		if _, err := teardown.Save(l, home, []byte("binary one"), pin, time.Now()); err == nil {
			t.Fatal("Save accepted a pin that names other bytes")
		}
		if exists(t, l.TeardownDir) {
			t.Error("the teardown directory was created")
		}
	})

	t.Run("writes nothing when the pin is invalid", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		pin.PinnedAt = "yesterday"
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err == nil {
			t.Fatal("Save accepted an invalid pin")
		}
		if exists(t, l.TeardownDir) {
			t.Error("the teardown directory was created")
		}
	})

	t.Run("sets the version itself", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		pin.Version = 0
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, found, err := teardown.ReadPin(l); err != nil || !found || got.Version != 1 {
			t.Fatalf("ReadPin = %+v, %v, %v", got, found, err)
		}
	})

	t.Run("drops the name of a merged-in binary that is gone", func(t *testing.T) {
		l, home := newControl(t)
		first := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build"})
		if _, err := teardown.Save(l, home, []byte("linux build"), first, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(teardown.BinaryPath(l, binstore.Hash([]byte("linux build")))); err != nil {
			t.Fatal(err)
		}
		second := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "mac build"})
		if _, err := teardown.Save(l, home, []byte("mac build"), teardown.Update(first, true, second).Pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		got, _, err := teardown.ReadPin(l)
		if err != nil {
			t.Fatal(err)
		}
		if got.Platforms["linux_amd64"].BinarySHA256 != "" || got.Platforms["linux_amd64"].ArchiveSHA256 == "" {
			t.Errorf("linux_amd64 = %+v: the missing binary is still named, or the archive digest was lost", got.Platforms["linux_amd64"])
		}
		if got.Platforms["darwin_arm64"].BinarySHA256 != binstore.Hash([]byte("mac build")) {
			t.Errorf("darwin_arm64 = %+v", got.Platforms["darwin_arm64"])
		}
	})

	t.Run("drops the name of a merged-in binary that a sweep takes after the rename", func(t *testing.T) {
		l, home := newControl(t)
		first := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build"})
		if _, err := teardown.Save(l, home, []byte("linux build"), first, time.Now()); err != nil {
			t.Fatal(err)
		}
		other := teardown.BinaryPath(l, binstore.Hash([]byte("linux build")))
		restore := teardown.SetAfterWritePinForTest(func(string) {
			if err := os.Remove(other); err != nil {
				t.Error(err)
			}
		})
		defer restore()
		second := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "mac build"})
		if _, err := teardown.Save(l, home, []byte("mac build"), teardown.Update(first, true, second).Pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		got, _, err := teardown.ReadPin(l)
		if err != nil {
			t.Fatal(err)
		}
		for _, hash := range got.BinaryHashes() {
			if !exists(t, teardown.BinaryPath(l, hash)) {
				t.Errorf("the pin names %s, which is not there", hash)
			}
		}
		if got.Platforms["linux_amd64"].BinarySHA256 != "" {
			t.Errorf("linux_amd64 = %+v", got.Platforms["linux_amd64"])
		}
	})

	t.Run("refuses a symlinked pin.json before installing anything, and never writes through it", func(t *testing.T) {
		l, home := newControl(t)
		if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(filepath.Dir(l.ControlHome), "elsewhere.json")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, l.PinPath); err != nil {
			t.Fatal(err)
		}
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		_, err := teardown.Save(l, home, []byte("b"), pin, time.Now())
		if err == nil || !strings.Contains(err.Error(), "Refusing symlinked or non-file managed path") {
			t.Fatalf("err = %v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "keep" {
			t.Errorf("wrote through the symlink: %q", got)
		}
		if exists(t, teardown.BinaryPath(l, binstore.Hash([]byte("b")))) {
			t.Error("the binary was installed despite the refusal")
		}
		if _, _, err := teardown.ReadPin(l); err == nil {
			t.Error("ReadPin followed a symlinked pin.json")
		}
	})

	t.Run("refuses a symlinked teardown directory", func(t *testing.T) {
		l, home := newControl(t)
		elsewhere := filepath.Join(filepath.Dir(l.ControlHome), "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(l.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, l.TeardownDir); err != nil {
			t.Fatal(err)
		}
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err == nil {
			t.Fatal("Save wrote through a symlinked teardown directory")
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("wrote %d entries through the symlink", len(entries))
		}
		if _, _, err := teardown.ReadPin(l); err == nil {
			t.Error("ReadPin read through a symlinked teardown directory")
		}
	})

	t.Run("does not sweep when the pin could not be written", func(t *testing.T) {
		l, home := newControl(t)
		stale := stageStale(t, l, time.Now())
		// pin.json becomes a directory: the write is refused.
		if err := os.Mkdir(l.PinPath, 0o700); err != nil {
			t.Fatal(err)
		}
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		if _, err := teardown.Save(l, home, []byte("b"), pin, time.Now()); err == nil {
			t.Fatal("Save wrote over a directory")
		}
		if !exists(t, stale) {
			t.Error("the sweep ran although the pin was not written")
		}
	})
}

// stageStale creates the teardown directory with a stored file no pin names,
// old enough for the sweep, and returns its path.
func stageStale(t *testing.T, l layout.Control, now time.Time) string {
	t.Helper()
	if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.TeardownDir, sum("stale"))
	if err := os.WriteFile(path, []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, path, now, time.Hour)
	return path
}

func TestReadPinIsDefensive(t *testing.T) {
	t.Run("an oversized pin.json is foreign, without being read whole", func(t *testing.T) {
		l, _ := newControl(t)
		if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.PinPath, []byte(`{"version": 1, "pad": "`+strings.Repeat("x", 70<<10)+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, found, err := teardown.ReadPin(l)
		if found || !errors.Is(err, teardown.ErrForeign) {
			t.Fatalf("found %v, err = %v; want a foreign refusal", found, err)
		}
	})

	t.Run("a FIFO where pin.json belongs is refused without hanging", func(t *testing.T) {
		l, _ := newControl(t)
		if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(l.PinPath, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := teardown.ReadPin(l)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a FIFO was read as a pin")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ReadPin hung on a FIFO")
		}
	})
}
