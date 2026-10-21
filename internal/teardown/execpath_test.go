package teardown_test

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/teardown"
)

func TestOpenBinary(t *testing.T) {
	l, home := newControl(t)
	content := "the pinned bytes"
	pin := filePin("b", content)
	if _, err := teardown.Save(l, home, []byte(content), pin, time.Now()); err != nil {
		t.Fatal(err)
	}
	stored := teardown.BinaryPath(l, sum(content))

	t.Run("returns a binary only for an intact file, and none otherwise", func(t *testing.T) {
		check, binary := teardown.OpenBinary(l, pin, "linux_arm64")
		if binary != nil || check.State != teardown.BinaryNone {
			t.Fatalf("check %+v, binary %v; want none and nil", check, binary)
		}
		check, binary = teardown.OpenBinary(l, pin, "linux_amd64")
		if binary == nil || check.State != teardown.BinaryOK || binary.Path != stored {
			t.Fatalf("check %+v, binary %+v; want ok at %s", check, binary, stored)
		}
		binary.Close()
		binary.Close() // twice is safe
		var none *teardown.Binary
		none.Close() // and so is a nil one
		if _, err := binary.ExecPath(); err == nil {
			t.Fatal("ExecPath of a closed binary succeeded")
		}
	})

	t.Run("the exec path names the bytes that were verified, not whatever the path names later", func(t *testing.T) {
		_, binary := teardown.OpenBinary(l, pin, "linux_amd64")
		if binary == nil {
			t.Fatal("not intact")
		}
		defer binary.Close()
		evil := stored + ".evil"
		if err := os.WriteFile(evil, []byte("evil"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(evil, stored); err != nil {
			t.Fatal(err)
		}
		path, err := binary.ExecPath()
		if runtime.GOOS != "linux" {
			// Without a way to execute a descriptor, a replaced file is refused.
			if err == nil {
				t.Fatalf("ExecPath = %q, want a refusal of a replaced file", path)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("exec path holds %q (%v), want the verified %q", got, err, content)
		}
	})
}
