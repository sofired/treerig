package teardown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/teardown"
)

func TestFilePin(t *testing.T) {
	digest := binstore.Hash([]byte("x"))
	got := teardown.FilePin("/home/me/treerig", "my-build", "linux_amd64", digest, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	got.Version = 1
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if !got.IsFile() || got.Source != "file:/home/me/treerig" || got.PinnedAt != "2026-10-14T12:00:00Z" || got.Platforms["linux_amd64"].BinarySHA256 != digest {
		t.Fatalf("pin = %+v", got)
	}
}

func TestValidSHA256(t *testing.T) {
	for s, want := range map[string]bool{strings.Repeat("a", 64): true, strings.Repeat("A", 64): false, strings.Repeat("a", 63): false, strings.Repeat("g", 64): false, "": false} {
		if got := teardown.ValidSHA256(s); got != want {
			t.Errorf("ValidSHA256(%q) = %v", s, got)
		}
	}
}

func TestInspectBinary(t *testing.T) {
	l, home := newControl(t)
	content := "the pinned bytes"
	pin := filePin("b", content)
	if _, err := teardown.Save(l, home, []byte(content), pin, time.Now()); err != nil {
		t.Fatal(err)
	}
	digest := binstore.Hash([]byte(content))
	stored := teardown.BinaryPath(l, digest)

	t.Run("ok", func(t *testing.T) {
		got := teardown.InspectBinary(l, pin, "linux_amd64")
		if got.State != teardown.BinaryOK || got.Path != stored || got.Pinned != digest || got.Found != digest {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("none for a platform the pin has no binary for", func(t *testing.T) {
		if got := teardown.InspectBinary(l, pin, "linux_arm64"); got != (teardown.BinaryCheck{State: teardown.BinaryNone}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("different bytes are a mismatch", func(t *testing.T) {
		if err := os.WriteFile(stored, []byte("other"), 0o700); err != nil {
			t.Fatal(err)
		}
		got := teardown.InspectBinary(l, pin, "linux_amd64")
		if got.State != teardown.BinaryMismatch || got.Found != binstore.Hash([]byte("other")) || !strings.HasPrefix(got.Problem, "has sha256 ") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a symlink is not followed, even to the right bytes", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real")
		if err := os.WriteFile(target, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(stored); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, stored); err != nil {
			t.Fatal(err)
		}
		got := teardown.InspectBinary(l, pin, "linux_amd64")
		if got.State != teardown.BinaryMismatch || got.Problem != "is not a regular file" || got.Found != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a missing file is missing", func(t *testing.T) {
		if err := os.Remove(stored); err != nil {
			t.Fatal(err)
		}
		got := teardown.InspectBinary(l, pin, "linux_amd64")
		if got.State != teardown.BinaryMissing || got.Problem != "is missing" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("an unreadable file is a mismatch that says why", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads anything")
		}
		if err := os.WriteFile(stored, []byte(content), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(stored, 0o700) })
		got := teardown.InspectBinary(l, pin, "linux_amd64")
		if got.State != teardown.BinaryMismatch || got.Problem != "cannot be read (permission denied)" {
			t.Fatalf("got %+v", got)
		}
	})
}
