package registry

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIdentityFromGitDirSkipsWhatCouldHangOrExhaustIt(t *testing.T) {
	plant := func(t *testing.T, write func(gitdir string) error) (commonDir, root string) {
		t.Helper()
		base := t.TempDir()
		commonDir = filepath.Join(base, "common")
		admin := filepath.Join(commonDir, "worktrees", "wt")
		if err := os.MkdirAll(admin, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := write(filepath.Join(admin, "gitdir")); err != nil {
			t.Fatal(err)
		}
		return commonDir, filepath.Join(base, "wt")
	}
	// within fails rather than hangs when a read blocks.
	within := func(t *testing.T, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); f() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("reading the gitdir file blocked")
		}
	}

	t.Run("a FIFO is skipped without blocking", func(t *testing.T) {
		commonDir, root := plant(t, func(gitdir string) error { return syscall.Mkfifo(gitdir, 0o600) })
		within(t, func() {
			if id, err := identityFromGitDir(commonDir, root); id != "" || err != nil {
				t.Errorf("identityFromGitDir = %q, %v; want the entry skipped", id, err)
			}
		})
	})

	t.Run("a file larger than one path is skipped", func(t *testing.T) {
		commonDir, root := plant(t, func(gitdir string) error {
			return os.WriteFile(gitdir, []byte(strings.Repeat("x", maxGitDirBytes+1)), 0o600)
		})
		if id, err := identityFromGitDir(commonDir, root); id != "" || err != nil {
			t.Fatalf("identityFromGitDir = %q, %v; want the entry skipped", id, err)
		}
	})
}
