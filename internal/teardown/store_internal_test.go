package teardown

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReadPinBytesRefusesWhatIsNotARegularFile calls readPinBytes directly, past
// the path-safety check ReadPin makes first, so the descriptor's own checks are
// what is tested: a FIFO swapped in after that check must not hang the open, and
// neither a FIFO nor a directory is read as a pin.
func TestReadPinBytesRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink to a perfectly good file: only O_NOFOLLOW refuses it.
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"a FIFO": fifo, "a directory": dir, "a symlink": link} {
		done := make(chan error, 1)
		go func() {
			_, err := readPinBytes(path)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, errPinNotRegular) {
				t.Errorf("%s: err = %v, want errPinNotRegular", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: readPinBytes did not return (the open blocked)", name)
		}
	}
}
