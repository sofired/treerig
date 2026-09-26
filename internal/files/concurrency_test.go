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
