package files_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// TestAtomicWriteConcurrency is a regression test for the fix that gave
// AtomicWrite's temp name a random suffix in addition to this process's PID
// (see files.go's own comment on AtomicWrite): before that fix, N goroutines
// racing to write the same destination shared one temp path, so all but the
// last to rename it failed with ENOENT once an earlier goroutine's rename
// had already moved it away. Reverting just the random suffix (back to
// fmt.Sprintf("%s.tmp.%d", destination, os.Getpid())) reproduces that
// failure here.
func TestAtomicWriteConcurrency(t *testing.T) {
	t.Run("N goroutines writing the same destination all succeed, and the file is never left half-written", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "shared.json")
		const n = 30
		payloads := make([]string, n)
		for i := range payloads {
			payloads[i] = fmt.Sprintf(`{"writer":%d,"padding":%q}`, i, strings.Repeat("x", 200))
		}

		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				errs[i] = files.AtomicWrite(destination, []byte(payloads[i]), 0o600)
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", i, err)
			}
		}

		contents, err := os.ReadFile(destination)
		requireNoError(t, err)
		matched := false
		for _, payload := range payloads {
			if string(contents) == payload {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("final content matched no single writer's payload (corrupted or mixed): %q", contents)
		}

		entries, err := os.ReadDir(root)
		requireNoError(t, err)
		if len(entries) != 1 || entries[0].Name() != "shared.json" {
			t.Fatalf("expected only the final file, no leftover temp files, got %v", entries)
		}
	})
}

// TestConcurrentControlHomeEstablishment is a regression test for the fix
// that made establishControlHome write its marker directly instead of
// through AtomicWrite's temp-then-rename (see files.go's own comment):
// before that fix, one goroutine's temp file, sitting in what should have
// been an empty directory, could make a concurrent goroutine's own
// AssertAdoptableControlHome check refuse the directory as "not empty and
// not created by berth." Reverting that one line (back to
// AtomicWrite(marker, []byte(ControlHomeFormat), 0o600)) reproduces that
// refusal here.
func TestConcurrentControlHomeEstablishment(t *testing.T) {
	t.Run("N goroutines securing the same brand new control home never refuse each other", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		control := layout.ControlLayout(home)
		userHome := filepath.Join(t.TempDir(), "user-home")

		const n = 20
		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				errs[i] = files.SecureControlDirectories(control, userHome)
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: %v", i, err)
			}
		}

		marker := filepath.Join(home, files.ControlHomeMarker)
		contents, err := os.ReadFile(marker)
		requireNoError(t, err)
		if string(contents) != files.ControlHomeFormat {
			t.Fatalf("got %q, want %q", contents, files.ControlHomeFormat)
		}
	})
}

// TestControlHomeMarkerSurvivesStaleReader is a deterministic regression
// test for the fix that made writeMarkerRacingCreators create the marker
// with O_EXCL instead of O_TRUNC (see files.go's own comment on
// writeMarkerRacingCreators and writeMarkerDirect): before that fix, a
// goroutine that had already read the marker as empty and the directory as
// unpopulated, then lost the race to establish the control home, truncated
// the winner's already-complete marker back to empty for the width of its
// own create-then-write — the exact window
// TestConcurrentControlHomeEstablishment's occasional failure on main
// (about 25 times in 200 runs) came from a third goroutine reading in.
//
// SetAfterMarkerReadForTest pins one goroutine ("the loser") at exactly that
// stale-read moment while a second, undisturbed call ("the winner") finishes
// establishing the same control home for real. The loser is then released
// into writeMarkerRacingCreators with the winner's marker already sitting
// there in full. This is deterministic, not a timing window to get lucky on
// like the original bug: on the fix, the loser's own create loses to EEXIST
// and it only ever reads the winner's content back (see
// writeMarkerRacingCreators), so the marker's mtime and size cannot change
// no matter how the loser is scheduled — this test asserts exactly that,
// rather than racing a reader to catch a truncation that might or might not
// land in a narrow window.
func TestControlHomeMarkerSurvivesStaleReader(t *testing.T) {
	t.Run("a goroutine that read the marker as empty before a winner finished never rewrites the winner's marker", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "control")
		control := layout.ControlLayout(home)
		userHome := filepath.Join(t.TempDir(), "user-home")
		marker := filepath.Join(home, files.ControlHomeMarker)

		paused := make(chan struct{})
		proceed := make(chan struct{})
		// Released on every exit, so a failed assertion below doesn't leave
		// the loser parked for the rest of the test binary.
		release := sync.OnceFunc(func() { close(proceed) })
		defer release()
		var mu sync.Mutex
		claimed := false
		restore := files.SetAfterMarkerReadForTest(func(controlHome string) {
			// Only the first call through — the loser — pauses here. The
			// winner's own call reaches this same hook too, once the
			// loser has been released, and must pass straight through:
			// sync.Once won't do, since Do blocks a second, concurrent
			// call until the first call's function returns, and the
			// loser's is deliberately parked on <-proceed — a second
			// caller would deadlock waiting on Once's own lock instead of
			// passing through. A plain mutex around the flag, released
			// before blocking, avoids that.
			mu.Lock()
			isLoser := !claimed
			claimed = true
			mu.Unlock()
			if isLoser {
				close(paused)
				<-proceed
			}
		})
		defer restore()

		loserErr := make(chan error, 1)
		go func() {
			loserErr <- files.SecureControlDirectories(control, userHome)
		}()

		<-paused // the loser has read populated=false and an empty/missing marker

		// The winner runs to completion on the same control home,
		// undisturbed: marker written in full, then repositories/ and
		// locks/ created — all while the loser is still paused above.
		if err := files.SecureControlDirectories(control, userHome); err != nil {
			t.Fatalf("winner: %v", err)
		}
		before, err := os.Stat(marker)
		requireNoError(t, err)
		data, err := os.ReadFile(marker)
		requireNoError(t, err)
		if string(data) != files.ControlHomeFormat {
			t.Fatalf("marker after winner = %q, want %q", data, files.ControlHomeFormat)
		}

		release() // release the loser into writeMarkerRacingCreators
		if err := <-loserErr; err != nil {
			t.Fatalf("loser: %v", err)
		}

		after, err := os.Stat(marker)
		requireNoError(t, err)
		if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatalf("marker touched by the loser: before %v (%d bytes), after %v (%d bytes)",
				before.ModTime(), before.Size(), after.ModTime(), after.Size())
		}
		data, err = os.ReadFile(marker)
		requireNoError(t, err)
		if string(data) != files.ControlHomeFormat {
			t.Fatalf("marker after loser = %q, want %q", data, files.ControlHomeFormat)
		}
	})
}
