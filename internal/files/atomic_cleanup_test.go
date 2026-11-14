package files_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/treerig/internal/files"
)

func TestAtomicWriteLeavesNothingWhenTheRenameFails(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "taken")
	// A directory with something in it can't be renamed over.
	if err := os.MkdirAll(filepath.Join(destination, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := files.AtomicWrite(destination, []byte("contents"), 0o600); err == nil {
		t.Fatal("AtomicWrite over a directory succeeded")
	}
	if left, _ := filepath.Glob(destination + ".tmp.*"); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}
