package teardown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/teardown"
)

func TestSweep(t *testing.T) {
	t.Run("removes old stored binaries the pin does not name and keeps those it does", func(t *testing.T) {
		l, home := newControl(t)
		now := time.Now()
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "mine", "darwin_arm64": "mine too"})
		if _, err := teardown.Save(l, home, []byte("mine"), pin, now); err != nil {
			t.Fatal(err)
		}
		if _, err := teardown.Save(l, home, []byte("mine too"), pin, now); err != nil {
			t.Fatal(err)
		}
		stale := stageStale(t, l, now)
		// Even long unused, a named binary stays: the pin names it.
		for _, content := range []string{"mine", "mine too"} {
			age(t, teardown.BinaryPath(l, binstore.Hash([]byte(content))), now, 24*time.Hour)
		}
		removed, err := teardown.Sweep(l, pin, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(removed) != 1 || removed[0] != filepath.Base(stale) || exists(t, stale) {
			t.Errorf("removed = %v, stale still there = %v", removed, exists(t, stale))
		}
		for _, content := range []string{"mine", "mine too"} {
			if !exists(t, teardown.BinaryPath(l, binstore.Hash([]byte(content)))) {
				t.Errorf("the pin's binary %q was swept", content)
			}
		}
		if !exists(t, l.PinPath) {
			t.Error("pin.json was swept")
		}
	})

	t.Run("Save sweeps after the rename: the replaced pin's binary goes once it is old", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now().Add(-time.Hour)
		first := releasePin("1.0.0", "old checksums", map[string]string{"linux_amd64": "old build"})
		if _, err := teardown.Save(l, home, []byte("old build"), first, start); err != nil {
			t.Fatal(err)
		}
		age(t, teardown.BinaryPath(l, binstore.Hash([]byte("old build"))), start, 0)
		second := releasePin("2.0.0", "new checksums", map[string]string{"linux_amd64": "new build"})
		saved, err := teardown.Save(l, home, []byte("new build"), second, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		oldPath := teardown.BinaryPath(l, binstore.Hash([]byte("old build")))
		if exists(t, oldPath) || len(saved.Removed) != 1 {
			t.Errorf("the replaced binary survived: removed = %v", saved.Removed)
		}
		if !exists(t, saved.BinaryPath) {
			t.Error("the new binary is gone")
		}
	})

	t.Run("Save keeps every binary the merged pin names, old or not", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now()
		first := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "linux build"})
		if _, err := teardown.Save(l, home, []byte("linux build"), first, start); err != nil {
			t.Fatal(err)
		}
		linux := teardown.BinaryPath(l, binstore.Hash([]byte("linux build")))
		age(t, linux, start, 24*time.Hour)

		second := releasePin("1.2.3", "checksums", map[string]string{"darwin_arm64": "mac build"})
		merged := teardown.Update(first, true, second)
		saved, err := teardown.Save(l, home, []byte("mac build"), merged.Pin, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !exists(t, linux) || len(saved.Removed) != 0 {
			t.Errorf("the merged pin's other binary was swept: removed = %v", saved.Removed)
		}
	})

	t.Run("keeps anything modified within the ten-minute grace, and removes it at ten minutes", func(t *testing.T) {
		if teardown.SweepGrace != 10*time.Minute {
			t.Fatalf("SweepGrace = %v", teardown.SweepGrace)
		}
		l, home := newControl(t)
		now := time.Now()
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "mine"})
		if _, err := teardown.Save(l, home, []byte("mine"), pin, now); err != nil {
			t.Fatal(err)
		}
		cases := map[string]time.Duration{
			"just written":       0,
			"nine minutes old":   9*time.Minute + 59*time.Second,
			"ten minutes old":    10 * time.Minute,
			"eleven minutes old": 11 * time.Minute,
			"in the future":      -time.Hour,
		}
		paths := map[string]string{}
		for name, d := range cases {
			path := filepath.Join(l.TeardownDir, sum(name))
			if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
				t.Fatal(err)
			}
			age(t, path, now, d)
			paths[name] = path
		}
		if _, err := teardown.Sweep(l, pin, now); err != nil {
			t.Fatal(err)
		}
		for name, wantGone := range map[string]bool{
			"just written": false, "nine minutes old": false, "ten minutes old": true, "eleven minutes old": true, "in the future": false,
		} {
			if gone := !exists(t, paths[name]); gone != wantGone {
				t.Errorf("%s: removed = %v, want %v", name, gone, wantGone)
			}
		}
	})

	t.Run("a binary installed moments ago by another writer survives this writer's sweep", func(t *testing.T) {
		l, home := newControl(t)
		now := time.Now()
		mine := releasePin("1.0.0", "mine", map[string]string{"linux_amd64": "mine"})
		// The other writer has installed but not yet renamed its pin.json.
		if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		other, err := binstore.Install(l.TeardownDir, []byte("theirs"), 0o700, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := teardown.Save(l, home, []byte("mine"), mine, now); err != nil {
			t.Fatal(err)
		}
		if !exists(t, other) {
			t.Error("a fresh install by a concurrent writer was swept")
		}
	})

	t.Run("considers only regular files named by 64 lowercase hex characters", func(t *testing.T) {
		l, home := newControl(t)
		now := time.Now()
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "mine"})
		if _, err := teardown.Save(l, home, []byte("mine"), pin, now); err != nil {
			t.Fatal(err)
		}
		hex := sum("x")
		outside := filepath.Join(filepath.Dir(l.ControlHome), "outside")
		if err := os.WriteFile(outside, []byte("do not touch"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(t, outside, now, 48*time.Hour)

		kept := map[string]func(path string){
			"uppercase hex":         func(p string) { writeOld(t, p, now) },
			"63 characters":         func(p string) { writeOld(t, p, now) },
			"65 characters":         func(p string) { writeOld(t, p, now) },
			"non-hex characters":    func(p string) { writeOld(t, p, now) },
			"a prefixed name":       func(p string) { writeOld(t, p, now) },
			"a suffixed name":       func(p string) { writeOld(t, p, now) },
			"a hex-named symlink":   func(p string) { mustSymlink(t, outside, p) },
			"a dangling symlink":    func(p string) { mustSymlink(t, filepath.Join(outside, "missing"), p) },
			"a hex-named directory": func(p string) { mustMkdir(t, p) },
		}
		names := map[string]string{
			"uppercase hex":         strings.ToUpper(hex),
			"63 characters":         hex[:63],
			"65 characters":         hex + "0",
			"non-hex characters":    strings.Repeat("g", 64),
			"a prefixed name":       "x" + hex[1:],
			"a suffixed name":       hex + ".bak",
			"a hex-named symlink":   sum("link"),
			"a dangling symlink":    sum("dangling"),
			"a hex-named directory": sum("dir"),
		}
		for name, create := range kept {
			create(filepath.Join(l.TeardownDir, names[name]))
		}
		// Everything is old enough for the sweep but for its kind: age the
		// symlinks themselves, which os.Chtimes would not.
		for name := range kept {
			path := filepath.Join(l.TeardownDir, names[name])
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				old := unix.NsecToTimespec(now.Add(-48 * time.Hour).UnixNano())
				if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{old, old}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					t.Fatal(err)
				}
			} else {
				age(t, path, now, 48*time.Hour)
			}
		}
		removed, err := teardown.Sweep(l, pin, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v", removed)
		}
		for name := range kept {
			if !exists(t, filepath.Join(l.TeardownDir, names[name])) {
				t.Errorf("%s was removed", name)
			}
		}
		if got, _ := os.ReadFile(outside); string(got) != "do not touch" {
			t.Errorf("the symlink's target changed: %q", got)
		}
		if !exists(t, l.PinPath) {
			t.Error("pin.json was removed")
		}
	})

	t.Run("a missing teardown directory is nothing to sweep", func(t *testing.T) {
		l, _ := newControl(t)
		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), time.Now())
		if err != nil || len(removed) != 0 {
			t.Fatalf("Sweep = %v, %v", removed, err)
		}
	})

	t.Run("refuses a symlinked teardown directory and removes nothing through it", func(t *testing.T) {
		l, _ := newControl(t)
		elsewhere := filepath.Join(filepath.Dir(l.ControlHome), "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(elsewhere, sum("victim"))
		writeOld(t, victim, time.Now())
		if err := os.MkdirAll(l.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, l.TeardownDir); err != nil {
			t.Fatal(err)
		}
		if _, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), time.Now()); err == nil {
			t.Fatal("Sweep followed a symlinked directory")
		}
		if !exists(t, victim) {
			t.Error("a file outside the control home was removed")
		}
	})

	t.Run("reports the first failure and removes nothing from a directory it cannot write", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		l, _ := newControl(t)
		now := time.Now()
		stale := stageStale(t, l, now)
		if err := os.Chmod(l.TeardownDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(l.TeardownDir, 0o700) })
		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now)
		if err == nil || len(removed) != 0 || !exists(t, stale) {
			t.Fatalf("Sweep = %v, %v; stale there = %v", removed, err, exists(t, stale))
		}
	})
}

// writeOld writes a day-old file at path.
func writeOld(t *testing.T, path string, now time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, path, now, 24*time.Hour)
}

// mustSymlink makes path a symlink to target.
func mustSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// mustMkdir makes the directory path.
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
