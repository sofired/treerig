package teardown_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/teardown"
)

func TestConcurrentWriters(t *testing.T) {
	t.Run("writers pinning at once leave one valid pin and every binary it names", func(t *testing.T) {
		pinConcurrently(t)
	})

	t.Run("two writers racing a sweep never lose a binary that is mid-install", func(t *testing.T) {
		l, home := newControl(t)
		// Stored files from the past exist for the sweep to find, so every
		// Save has real work to do while the other installs.
		now := time.Now()
		for i := 0; i < 20; i++ {
			writeOldAt(t, l, fmt.Sprintf("stale %d", i), now)
		}
		var group errgroup.Group
		for w := 0; w < 2; w++ {
			group.Go(func() error {
				for r := 0; r < 40; r++ {
					content := fmt.Sprintf("writer %d binary %d", w, r)
					pin := releasePin("1.0.0", fmt.Sprintf("checksums %d", w), map[string]string{"linux_amd64": content})
					saved, err := teardown.Save(l, home, []byte(content), pin, time.Now())
					if err != nil {
						return err
					}
					// The pin this writer just wrote names its binary; unless a
					// later writer replaced the pin, that binary must be there,
					// and either way it is inside the grace window.
					if _, err := os.Stat(saved.BinaryPath); err != nil {
						return fmt.Errorf("writer %d lost its own binary: %w", w, err)
					}
				}
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			t.Fatal(err)
		}
		pin, _, err := teardown.ReadPin(l)
		if err != nil {
			t.Fatal(err)
		}
		for _, hash := range pin.BinaryHashes() {
			if !exists(t, teardown.BinaryPath(l, hash)) {
				t.Errorf("the final pin names %s, which is gone", hash)
			}
		}
		for i := 0; i < 20; i++ {
			if exists(t, filepath.Join(l.TeardownDir, sum(fmt.Sprintf("stale %d", i)))) {
				t.Errorf("stale file %d was never swept", i)
			}
		}
	})
}

// writeOldAt writes a stored file named for name, an hour old.
func writeOldAt(t *testing.T, l layout.Control, name string, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.TeardownDir, sum(name))
	if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, path, now, time.Hour)
}

// pinConcurrently runs writers that each read, update and save a pin in a loop
// while a reader reads it, then checks the final state.
func pinConcurrently(t *testing.T) {
	t.Helper()
	l, home := newControl(t)
	const writers, rounds = 8, 25

	var group errgroup.Group
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readErrs := make(chan error, 1)
	// A reader sees the old pin or the new one, never a refusal, a mixture or a
	// pin whose binary is not there yet.
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			pin, _, err := teardown.ReadPin(l)
			// Whatever pin is read, the binaries it names are already
			// installed: the binary goes first, then the rename.
			for _, hash := range pin.BinaryHashes() {
				if _, statErr := os.Stat(teardown.BinaryPath(l, hash)); statErr != nil && err == nil {
					err = fmt.Errorf("the pin names %s before it is installed: %w", hash, statErr)
				}
			}
			if err != nil {
				select {
				case readErrs <- err:
				default:
				}
				return
			}
		}
	}()

	for w := 0; w < writers; w++ {
		group.Go(func() error {
			for r := 0; r < rounds; r++ {
				// Half the writers pin the same release (merging on another
				// platform), the rest each their own.
				release, checksums := "1.0.0", "shared"
				if w%2 == 1 {
					release, checksums = fmt.Sprintf("2.%d.0", w), fmt.Sprintf("own %d", w)
				}
				platform := teardown.ReleasePlatforms[w%len(teardown.ReleasePlatforms)]
				content := fmt.Sprintf("binary %d round %d", w, r)
				incoming := releasePin(release, checksums, map[string]string{platform: content})

				existing, found, err := teardown.ReadPin(l)
				if err != nil {
					return fmt.Errorf("writer %d read: %w", w, err)
				}
				outcome := teardown.Update(existing, found, incoming)
				saved, err := teardown.Save(l, home, []byte(content), outcome.Pin, time.Now())
				if err != nil {
					return fmt.Errorf("writer %d save: %w", w, err)
				}
				if saved.SweepErr != nil {
					return fmt.Errorf("writer %d sweep: %w", w, saved.SweepErr)
				}
			}
			return nil
		})
	}
	err := group.Wait()
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readErrs:
		t.Fatalf("a reader met a broken pin: %v", err)
	default:
	}

	pin, found, err := teardown.ReadPin(l)
	if err != nil || !found {
		t.Fatalf("final ReadPin: %v, %v", found, err)
	}
	hashes := pin.BinaryHashes()
	if len(hashes) == 0 {
		t.Fatal("the final pin names no binary")
	}
	for _, hash := range hashes {
		data, err := os.ReadFile(teardown.BinaryPath(l, hash))
		if err != nil {
			t.Errorf("a binary the pin names is missing: %v", err)
			continue
		}
		if binstore.Hash(data) != hash {
			t.Errorf("binary %s holds other bytes", hash)
		}
		if fileMode(t, teardown.BinaryPath(l, hash)) != 0o700 {
			t.Errorf("binary %s has mode %o", hash, fileMode(t, teardown.BinaryPath(l, hash)))
		}
	}
	// Nothing but the pin and 64-hex files is left behind: no temporary file.
	entries, err := os.ReadDir(l.TeardownDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "pin.json" && len(entry.Name()) != 64 {
			t.Errorf("stray entry %q", entry.Name())
		}
	}
	if fileMode(t, l.PinPath) != 0o600 {
		t.Errorf("pin.json mode = %o", fileMode(t, l.PinPath))
	}
}
