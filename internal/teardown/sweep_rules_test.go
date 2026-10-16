package teardown_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/teardown"
)

func TestSweepRules(t *testing.T) {
	t.Run("removes the temporary files of killed writers under the same rules, matching AtomicWrite's real names", func(t *testing.T) {
		l, home := newControl(t)
		if err := files.SecureTeardownDirectory(l, home); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		// files.AtomicWrite leaves its temporary file behind when the rename
		// fails: here, because the destination is a non-empty directory. That
		// gives the real name, not one this test spelled.
		var real []string
		for _, destination := range []string{filepath.Join(l.TeardownDir, sum("a binary")), l.PinPath} {
			if err := os.MkdirAll(filepath.Join(destination, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := files.AtomicWrite(destination, []byte("partial"), 0o700); err == nil {
				t.Fatal("AtomicWrite over a directory succeeded")
			}
			matches, _ := filepath.Glob(destination + ".tmp.*")
			if len(matches) != 1 {
				t.Fatalf("leftovers for %s: %v", destination, matches)
			}
			real = append(real, matches[0])
			if err := os.RemoveAll(destination); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range real {
			age(t, path, now, time.Hour)
		}
		pin := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "mine"})
		if err := teardown.WritePin(l, pin); err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(l.TeardownDir, sum("fresh")+".tmp.1.abcd1234")
		if err := os.WriteFile(fresh, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := teardown.Sweep(l, pin, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(removed) != 2 {
			t.Errorf("removed = %v", removed)
		}
		for _, path := range real {
			if exists(t, path) {
				t.Errorf("the leftover %s survived", filepath.Base(path))
			}
		}
		if !exists(t, fresh) || !exists(t, l.PinPath) {
			t.Error("a fresh temporary file or pin.json was removed")
		}
	})

	t.Run("leaves anything else that merely looks like a temporary file", func(t *testing.T) {
		l, _ := newControl(t)
		if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		hex := sum("x")
		outside := filepath.Join(filepath.Dir(l.ControlHome), "outside")
		if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		names := []string{
			hex[:63] + ".tmp.1.abcd", hex + ".tmp.x.abcd", hex + ".tmp.1.", hex + ".tmp.1.ABCD", hex + ".tmp.1.ab.cd",
			hex + ".tmp", "pin.json.tmp.1", "pin.json.tmp.x.ab", "pin.json.bak", "pin.JSON.tmp.1.ab", "other.json.tmp.1.ab",
			"x" + hex + ".tmp.1.ab", hex + ".tmp.1.ab.", "settings.tmp.1.ab",
		}
		for _, name := range names {
			writeOld(t, filepath.Join(l.TeardownDir, name), now)
		}
		link := filepath.Join(l.TeardownDir, hex+".tmp.2.linked")
		mustSymlink(t, outside, link)
		dir := filepath.Join(l.TeardownDir, hex+".tmp.3.dir")
		mustMkdir(t, dir)
		age(t, dir, now, 48*time.Hour)

		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now)
		if err != nil || len(removed) != 0 {
			t.Fatalf("Sweep = %v, %v", removed, err)
		}
		for _, name := range append(names, filepath.Base(link), filepath.Base(dir)) {
			if !exists(t, filepath.Join(l.TeardownDir, name)) {
				t.Errorf("%s was removed", name)
			}
		}
	})

	t.Run("keeps what the pin on disk names, as well as what the caller's pin names", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now()
		// Writer A's pin is on disk, and its binary is old.
		onDisk := releasePin("1.0.0", "a", map[string]string{"linux_amd64": "writer a"})
		if _, err := teardown.Save(l, home, []byte("writer a"), onDisk, start); err != nil {
			t.Fatal(err)
		}
		a := teardown.BinaryPath(l, binstore.Hash([]byte("writer a")))
		age(t, a, start, 3*time.Hour)
		// Writer B sweeps with a pin of its own that does not name A's binary.
		mine := releasePin("2.0.0", "b", map[string]string{"linux_amd64": "writer b"})
		removed, err := teardown.Sweep(l, mine, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if !exists(t, a) || len(removed) != 0 {
			t.Errorf("a binary the on-disk pin names was swept: removed = %v", removed)
		}
	})

	t.Run("keeps what the caller's pin names even when the pin on disk does not", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now()
		onDisk := releasePin("1.0.0", "a", map[string]string{"linux_amd64": "on disk"})
		if _, err := teardown.Save(l, home, []byte("on disk"), onDisk, start); err != nil {
			t.Fatal(err)
		}
		mine, err := binstore.Install(l.TeardownDir, []byte("mine"), 0o700, start)
		if err != nil {
			t.Fatal(err)
		}
		age(t, mine, start, 3*time.Hour)
		removed, err := teardown.Sweep(l, releasePin("2.0.0", "b", map[string]string{"linux_amd64": "mine"}), start)
		if err != nil || len(removed) != 0 || !exists(t, mine) {
			t.Fatalf("Sweep = %v, %v; mine there = %v", removed, err, exists(t, mine))
		}
	})

	t.Run("removes nothing when the pin on disk cannot be read, whatever the reason", func(t *testing.T) {
		for name, body := range map[string]string{"foreign": `{"version":0}`, "newer": `{"version":2}`, "garbage": `{nope`, "an unknown key": `{"version":1,"x":1}`} {
			t.Run(name, func(t *testing.T) {
				l, _ := newControl(t)
				now := time.Now()
				stale := stageStale(t, l, now)
				tmp := filepath.Join(l.TeardownDir, sum("t")+".tmp.1.abcd")
				writeOld(t, tmp, now)
				writeRaw(t, l, []byte(body))
				removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now)
				if err == nil || len(removed) != 0 || !exists(t, stale) || !exists(t, tmp) {
					t.Fatalf("Sweep = %v, %v; stale there = %v, tmp there = %v", removed, err, exists(t, stale), exists(t, tmp))
				}
				if got, _ := os.ReadFile(l.PinPath); string(got) != body {
					t.Errorf("the pin was changed: %q", got)
				}
			})
		}
	})

	t.Run("removes nothing when pin.json is unreadable or not a regular file", func(t *testing.T) {
		l, _ := newControl(t)
		now := time.Now()
		stale := stageStale(t, l, now)
		mustMkdir(t, l.PinPath)
		if removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now); err == nil || len(removed) != 0 || !exists(t, stale) {
			t.Fatalf("directory: Sweep = %v, %v", removed, err)
		}
		if err := os.Remove(l.PinPath); err != nil {
			t.Fatal(err)
		}
		if os.Geteuid() == 0 {
			return
		}
		writeRaw(t, l, []byte(`{"version":1}`))
		if err := os.Chmod(l.PinPath, 0); err != nil {
			t.Fatal(err)
		}
		if removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now); err == nil || len(removed) != 0 || !exists(t, stale) {
			t.Fatalf("unreadable: Sweep = %v, %v", removed, err)
		}
	})

	t.Run("a missing pin file counts as no pin: the sweep goes on", func(t *testing.T) {
		l, _ := newControl(t)
		now := time.Now()
		stale := stageStale(t, l, now)
		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now)
		if err != nil || len(removed) != 1 || exists(t, stale) {
			t.Fatalf("Sweep = %v, %v", removed, err)
		}
	})

	t.Run("looks again just before removing: a file that became fresh is kept", func(t *testing.T) {
		l, _ := newControl(t)
		now := time.Now()
		stale := stageStale(t, l, now)
		restore := teardown.SetBeforeRemoveForTest(func(path string) {
			// Another writer installs the same content: Install refreshes it.
			age(t, path, now, 0)
		})
		defer restore()
		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now)
		if err != nil || len(removed) != 0 || !exists(t, stale) {
			t.Fatalf("Sweep = %v, %v; stale there = %v", removed, err, exists(t, stale))
		}
	})

	t.Run("looks again just before removing: a file that became a symlink is kept, and so is its target", func(t *testing.T) {
		l, _ := newControl(t)
		now := time.Now()
		stale := stageStale(t, l, now)
		outside := filepath.Join(filepath.Dir(l.ControlHome), "outside")
		if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		restore := teardown.SetBeforeRemoveForTest(func(path string) {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Error(err)
			}
			// An old symlink: only its kind can save it now.
			age(t, outside, now, 48*time.Hour)
		})
		defer restore()
		removed, err := teardown.Sweep(l, releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"}), now.Add(48*time.Hour))
		if err != nil || len(removed) != 0 || !exists(t, stale) {
			t.Fatalf("Sweep = %v, %v; link there = %v", removed, err, exists(t, stale))
		}
		if got, _ := os.ReadFile(outside); string(got) != "keep" {
			t.Errorf("target = %q", got)
		}
	})
}

func TestSaveSurvivesASweepThatRemovesItsBinary(t *testing.T) {
	t.Run("after the rename Save installs its binary again if a sweep took it", func(t *testing.T) {
		l, home := newControl(t)
		restore := teardown.SetAfterWritePinForTest(func(binaryPath string) {
			if err := os.Remove(binaryPath); err != nil {
				t.Error(err)
			}
		})
		defer restore()
		pin := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "mine"})
		saved, err := teardown.Save(l, home, []byte("mine"), pin, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := os.ReadFile(saved.BinaryPath)
		if readErr != nil || string(got) != "mine" {
			t.Fatalf("binary = %q, %v", got, readErr)
		}
		if fileMode(t, saved.BinaryPath) != 0o700 {
			t.Errorf("mode = %o", fileMode(t, saved.BinaryPath))
		}
	})
}

func TestStalledWriter(t *testing.T) {
	t.Run("a writer stalled past the grace leaves a pin that fails closed, and pinning again repairs it", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now()
		old := releasePin("1.0.0", "old", map[string]string{"linux_amd64": "old build"})
		if _, err := teardown.Save(l, home, []byte("old build"), old, start); err != nil {
			t.Fatal(err)
		}
		// Writer A installs its binary and then stalls for longer than the grace.
		stalled, err := binstore.Install(l.TeardownDir, []byte("stalled build"), 0o700, start)
		if err != nil {
			t.Fatal(err)
		}
		age(t, stalled, start, teardown.SweepGrace+time.Minute)
		// Writer B saves meanwhile, and its sweep takes the binary no pin names.
		other := releasePin("1.1.0", "other", map[string]string{"linux_amd64": "other build"})
		if saved, err := teardown.Save(l, home, []byte("other build"), other, start); err != nil || saved.SweepErr != nil {
			t.Fatalf("Save = %+v, %v", saved, err)
		}
		if exists(t, stalled) {
			t.Fatal("the stalled writer's binary survived the sweep, so there is no stall to test")
		}
		// A wakes up and renames its pin.
		mine := releasePin("1.2.0", "mine", map[string]string{"linux_amd64": "stalled build"})
		if err := teardown.WritePin(l, mine); err != nil {
			t.Fatal(err)
		}
		// The pin reads, and names a binary that is not there: the gate's hash
		// check cannot pass, so removal refuses.
		got, found, err := teardown.ReadPin(l)
		if err != nil || !found {
			t.Fatalf("ReadPin = %v, %v", found, err)
		}
		hashes := got.BinaryHashes()
		if len(hashes) != 1 || exists(t, teardown.BinaryPath(l, hashes[0])) {
			t.Fatalf("the pin does not dangle: %v", hashes)
		}
		// Pinning again repairs it.
		if _, err := teardown.Save(l, home, []byte("stalled build"), mine, time.Now()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(teardown.BinaryPath(l, hashes[0]))
		if err != nil || string(data) != "stalled build" {
			t.Fatalf("after the re-pin: %q, %v", data, err)
		}
	})
}

func TestKilledWriter(t *testing.T) {
	t.Run("the state a SIGKILL leaves: the old pin still reads, and the sweep removes only the orphans", func(t *testing.T) {
		l, home := newControl(t)
		start := time.Now()
		old := releasePin("1.0.0", "old", map[string]string{"linux_amd64": "old build"})
		if _, err := teardown.Save(l, home, []byte("old build"), old, start); err != nil {
			t.Fatal(err)
		}
		oldBinary := teardown.BinaryPath(l, binstore.Hash([]byte("old build")))

		// Killed after the install, before the rename: a binary no pin names.
		orphan, err := binstore.Install(l.TeardownDir, []byte("new build"), 0o700, start)
		if err != nil {
			t.Fatal(err)
		}
		// Killed inside AtomicWrite: temporary files of both kinds.
		tmpBinary := filepath.Join(l.TeardownDir, sum("half written")+".tmp.4242.deadbeef")
		tmpPin := filepath.Join(l.PinPath + ".tmp.4242.deadbeef")
		for _, path := range []string{tmpBinary, tmpPin} {
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		pinBefore, _ := os.ReadFile(l.PinPath)

		got, found, err := teardown.ReadPin(l)
		if err != nil || !found || got.Release != "1.0.0" {
			t.Fatalf("ReadPin after the kill = %+v, %v, %v", got, found, err)
		}

		// Inside the grace window nothing is touched.
		removed, err := teardown.Sweep(l, got, start.Add(time.Minute))
		if err != nil || len(removed) != 0 {
			t.Fatalf("early Sweep = %v, %v", removed, err)
		}
		// Past it, only the orphans go.
		removed, err = teardown.Sweep(l, got, start.Add(time.Hour))
		if err != nil || len(removed) != 3 {
			t.Fatalf("Sweep = %v, %v", removed, err)
		}
		for _, path := range []string{orphan, tmpBinary, tmpPin} {
			if exists(t, path) {
				t.Errorf("%s survived", filepath.Base(path))
			}
		}
		if !exists(t, oldBinary) {
			t.Error("the old pin's binary was swept")
		}
		if after, _ := os.ReadFile(l.PinPath); string(after) != string(pinBefore) {
			t.Error("pin.json changed")
		}
	})
}
