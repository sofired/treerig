package binstore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// TestInstall covers content-addressing: same bytes install once and share
// a path, different bytes get different paths, a second install of
// existing content refreshes its modification time instead of rewriting
// it, the installed file is exactly what was given, and a concurrent
// Prune (in another process) racing that same refresh away — reached
// through binstore.SetBeforeChtimesForTest, the package's own test-only
// seam for that exact window — is recovered from rather than failed.
func TestInstall(t *testing.T) {
	t.Run("installs content under its own hash", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path, err := binstore.Install(dir, []byte("a berth build"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if filepath.Dir(path) != dir {
			t.Fatalf("path %q is not directly under %q", path, dir)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "a berth build" {
			t.Fatalf("content = %q", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
		}
	})

	t.Run("installing the same content twice refreshes its modification time rather than rewriting it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		first, err := binstore.Install(dir, []byte("same bytes"), 0o700, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("first Install: %v", err)
		}
		later := time.Now()
		second, err := binstore.Install(dir, []byte("same bytes"), 0o700, later)
		if err != nil {
			t.Fatalf("second Install: %v", err)
		}
		if second != first {
			t.Fatalf("second path %q != first %q", second, first)
		}
		info, err := os.Stat(second)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if !info.ModTime().Equal(later) {
			t.Fatalf("modification time = %v, want the second Install's own %v (refreshed, not left as the first's)", info.ModTime(), later)
		}
		got, err := os.ReadFile(second)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "same bytes" {
			t.Fatalf("content changed on the refresh: %q", got)
		}
	})

	t.Run("replaces an entry under the right name whose bytes or permissions are wrong", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			content string
			mode    os.FileMode
		}{
			{"different bytes", "something else entirely", 0o700},
			{"same length, different bytes", "a berth bui1d", 0o700},
			{"right bytes, wrong permissions", "a berth build", 0o600},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := registrytest.TempDir(t)
				want := []byte("a berth build")
				target := binstore.Path(dir, binstore.Hash(want))
				if err := os.WriteFile(target, []byte(tc.content), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, tc.mode); err != nil { // past the umask
					t.Fatal(err)
				}
				path, err := binstore.Install(dir, want, 0o700, time.Now())
				if err != nil {
					t.Fatalf("Install: %v", err)
				}
				if path != target {
					t.Fatalf("path = %q, want %q", path, target)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) || info.Mode().Perm() != 0o700 {
					t.Fatalf("after Install: content %q, mode %v; want %q, 0700", got, info.Mode().Perm(), want)
				}
			})
		}
	})

	t.Run("different content installs under different paths", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		a, err := binstore.Install(dir, []byte("one"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install a: %v", err)
		}
		b, err := binstore.Install(dir, []byte("two"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install b: %v", err)
		}
		if a == b {
			t.Fatalf("expected different content to install under different paths, both got %q", a)
		}
	})

	t.Run("recovers when an entry vanishes between the exists check and Chtimes", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		content := []byte("raced")
		first, err := binstore.Install(dir, content, 0o700, time.Now())
		if err != nil {
			t.Fatalf("first Install: %v", err)
		}

		restore := binstore.SetBeforeChtimesForTest(func(target string) {
			// Simulates a concurrent process's Prune winning the race: the
			// entry Install is about to revive is gone by the time it
			// gets to Chtimes.
			if err := os.Remove(target); err != nil {
				t.Fatalf("removing %s to simulate a concurrent Prune: %v", target, err)
			}
		})
		defer restore()

		second, err := binstore.Install(dir, content, 0o700, time.Now())
		if err != nil {
			t.Fatalf("second Install (after the simulated race): %v", err)
		}
		if second != first {
			t.Fatalf("second path %q != first %q", second, first)
		}
		got, err := os.ReadFile(second)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "raced" {
			t.Fatalf("content = %q, want %q", got, "raced")
		}
	})
}

// bigCap is a maxEntries far past anything these tests install, so a
// Prune call that means to test the age rule alone is not also, by
// accident, testing the count cap.
const bigCap = 1000

// noGrace is a grace window of zero, for a Prune call that means to test
// the age or count rule alone, undisturbed by the separate grace rule
// (which its own tests below cover directly).
const noGrace = time.Duration(0)

// TestPrune covers three independent rules, and their interaction: age (an
// entry older than the threshold goes, keep and anything younger stays),
// count (once there are more than maxEntries entries younger than the
// threshold, the oldest-used of them go too, regardless of their age), and
// grace (an entry younger than the grace window is untouched by either of
// the other two) — plus keep's own immunity to all three, a missing
// directory being a no-op, and the sweep continuing past one entry it
// cannot remove.
func TestPrune(t *testing.T) {
	t.Run("removes an entry older than the threshold, keeping the one named", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		keep, err := binstore.Install(dir, []byte("keep me"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		old, err := binstore.Install(dir, []byte("prune me"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install old: %v", err)
		}
		past := time.Now().Add(-60 * 24 * time.Hour)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		if err := binstore.Prune(dir, keep, 30*24*time.Hour, bigCap, noGrace, time.Now()); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("kept entry is gone: %v", err)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("old entry survived Prune: err=%v", err)
		}
	})

	t.Run("leaves an entry younger than the threshold alone", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		young, err := binstore.Install(dir, []byte("still fresh"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if err := binstore.Prune(dir, "", 30*24*time.Hour, bigCap, noGrace, time.Now()); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(young); err != nil {
			t.Fatalf("young entry was removed: %v", err)
		}
	})

	t.Run("a store directory that does not exist yet is not an error", func(t *testing.T) {
		dir := filepath.Join(registrytest.TempDir(t), "never-created")
		if err := binstore.Prune(dir, "", 30*24*time.Hour, bigCap, noGrace, time.Now()); err != nil {
			t.Fatalf("Prune: %v", err)
		}
	})

	t.Run("evicts the oldest-used entries past the count cap, even though age alone would keep them", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		now := time.Now()
		keep, err := binstore.Install(dir, []byte("current"), 0o700, now)
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		// Three others, all well within the age threshold, oldest to
		// newest, so the cap alone decides which survive.
		var others []string
		for i, label := range []string{"oldest", "middle", "newest"} {
			path, err := binstore.Install(dir, []byte(label), 0o700, now.Add(time.Duration(i)*time.Minute))
			if err != nil {
				t.Fatalf("Install %s: %v", label, err)
			}
			others = append(others, path)
		}

		if err := binstore.Prune(dir, keep, 30*24*time.Hour, 2, noGrace, now.Add(time.Hour)); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("kept entry is gone: %v", err)
		}
		if _, err := os.Stat(others[0]); !os.IsNotExist(err) {
			t.Fatalf("oldest entry should have been evicted by the count cap: err=%v", err)
		}
		if _, err := os.Stat(others[1]); err != nil {
			t.Fatalf("middle entry should have survived within the cap: %v", err)
		}
		if _, err := os.Stat(others[2]); err != nil {
			t.Fatalf("newest entry should have survived within the cap: %v", err)
		}
	})

	t.Run("keep is never evicted by the count cap, however old or however many others there are", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		now := time.Now()
		keep, err := binstore.Install(dir, []byte("current"), 0o700, now.Add(-60*24*time.Hour))
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		if _, err := binstore.Install(dir, []byte("one more"), 0o700, now); err != nil {
			t.Fatalf("Install: %v", err)
		}

		if err := binstore.Prune(dir, keep, 30*24*time.Hour, 0, noGrace, now); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("kept entry was evicted despite being named keep: %v", err)
		}
	})

	t.Run("an entry inside the grace window survives the age rule even though it is older than the threshold", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		now := time.Now()
		keep, err := binstore.Install(dir, []byte("keep me"), 0o700, now)
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		// Two seconds old — the age rule's own one-second threshold alone
		// would evict it, but ten minutes of grace does not.
		recent, err := binstore.Install(dir, []byte("just installed"), 0o700, now.Add(-2*time.Second))
		if err != nil {
			t.Fatalf("Install recent: %v", err)
		}

		if err := binstore.Prune(dir, keep, time.Second, bigCap, 10*time.Minute, now); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Fatalf("entry inside the grace window was evicted by the age rule: %v", err)
		}
	})

	t.Run("an entry inside the grace window survives the count cap even past it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		now := time.Now()
		keep, err := binstore.Install(dir, []byte("keep me"), 0o700, now)
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		recent, err := binstore.Install(dir, []byte("just installed"), 0o700, now)
		if err != nil {
			t.Fatalf("Install recent: %v", err)
		}

		// maxEntries 0: the count rule alone would evict every non-keep
		// entry, however young, if grace did not protect it first.
		if err := binstore.Prune(dir, keep, 30*24*time.Hour, 0, 10*time.Minute, now); err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Fatalf("entry inside the grace window was evicted by the count cap: %v", err)
		}
	})

	t.Run("keeps sweeping after an entry that cannot be removed, and reports its error", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		keep, err := binstore.Install(dir, []byte("keep me"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}
		removable, err := binstore.Install(dir, []byte("goes fine"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install removable: %v", err)
		}
		past := time.Now().Add(-60 * 24 * time.Hour)
		if err := os.Chtimes(removable, past, past); err != nil {
			t.Fatalf("Chtimes removable: %v", err)
		}
		// A non-empty directory where an entry would be: os.Remove refuses
		// it (ENOTEMPTY), the way a permission-refused entry would refuse
		// it too — either way, Prune's own contract is the same: report it,
		// do not let it stop the rest of the sweep.
		stuck := filepath.Join(dir, "stuck-entry")
		if err := os.MkdirAll(stuck, 0o700); err != nil {
			t.Fatalf("MkdirAll stuck: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stuck, "inside"), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile inside stuck: %v", err)
		}
		if err := os.Chtimes(stuck, past, past); err != nil {
			t.Fatalf("Chtimes stuck: %v", err)
		}

		err = binstore.Prune(dir, keep, 30*24*time.Hour, bigCap, noGrace, time.Now())
		if err == nil {
			t.Fatalf("expected Prune to report the entry it could not remove")
		}
		if _, statErr := os.Stat(keep); statErr != nil {
			t.Fatalf("kept entry is gone: %v", statErr)
		}
		if _, statErr := os.Stat(removable); !os.IsNotExist(statErr) {
			t.Fatalf("removable entry should have been swept despite the other failing: stat err=%v", statErr)
		}
		if _, statErr := os.Stat(stuck); statErr != nil {
			t.Fatalf("stuck entry should still be there, since it could not be removed: %v", statErr)
		}
	})

	t.Run("reports the age-rule entry's error over the count-rule entry's when both fail, since it is processed first", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		keep, err := binstore.Install(dir, []byte("keep me"), 0o700, time.Now())
		if err != nil {
			t.Fatalf("Install keep: %v", err)
		}

		// Old enough for the age rule alone to evict it outright — the
		// first phase Prune builds its removal list from.
		ageStuck := filepath.Join(dir, "stuck-age-entry")
		if err := os.MkdirAll(ageStuck, 0o700); err != nil {
			t.Fatalf("MkdirAll ageStuck: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ageStuck, "inside"), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile inside ageStuck: %v", err)
		}
		past := time.Now().Add(-60 * 24 * time.Hour)
		if err := os.Chtimes(ageStuck, past, past); err != nil {
			t.Fatalf("Chtimes ageStuck: %v", err)
		}

		// Young — only the count rule (maxEntries 0, below) evicts it, in
		// the second phase, after every age-rule removal is already queued.
		countStuck := filepath.Join(dir, "stuck-count-entry")
		if err := os.MkdirAll(countStuck, 0o700); err != nil {
			t.Fatalf("MkdirAll countStuck: %v", err)
		}
		if err := os.WriteFile(filepath.Join(countStuck, "inside"), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile inside countStuck: %v", err)
		}

		err = binstore.Prune(dir, keep, 30*24*time.Hour, 0, noGrace, time.Now())
		if err == nil {
			t.Fatalf("expected Prune to report an error")
		}
		if !strings.Contains(err.Error(), "stuck-age-entry") {
			t.Fatalf("error = %q, want it naming the age-rule entry (queued for removal first)", err.Error())
		}
		if strings.Contains(err.Error(), "stuck-count-entry") {
			t.Fatalf("error = %q, should name only the first failure, not the count-rule entry's too", err.Error())
		}
		if _, statErr := os.Stat(keep); statErr != nil {
			t.Fatalf("kept entry is gone: %v", statErr)
		}
		if _, statErr := os.Stat(ageStuck); statErr != nil {
			t.Fatalf("age-rule entry should still be there, since it could not be removed: %v", statErr)
		}
		if _, statErr := os.Stat(countStuck); statErr != nil {
			t.Fatalf("count-rule entry should still be there, since it could not be removed: %v", statErr)
		}
	})
}

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := binstore.HashFile(path)
	if err != nil || got != binstore.Hash([]byte("abc")) {
		t.Fatalf("HashFile = %q, %v", got, err)
	}
	if _, err := binstore.HashFile(path + ".none"); err == nil {
		t.Fatal("a missing file hashed")
	}
}
