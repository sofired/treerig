//go:build !linux

package teardown

import (
	"errors"
	"fmt"
	"os"
)

// ExecPath is the path to hand execve. Without a way to execute a descriptor
// (macOS has no fexecve), the best this can do is the stored path, after
// confirming that it still names the file this Binary hashed: a rename or
// replacement since then refuses. A swap in the instants between this check
// and the exec is not excluded here, and is within the threat model's
// accepted limit (docs/design/teardown.md, "Platforms"): the directory is
// 0700 and a process that can write it can rewrite the pin.
func (b *Binary) ExecPath() (string, error) {
	if b == nil || b.file == nil {
		return "", errors.New("the verified binary is closed")
	}
	opened, err := b.file.Stat()
	if err != nil {
		return "", fmt.Errorf("could not check the verified binary: %w", err)
	}
	now, err := os.Lstat(b.Path)
	if err != nil {
		return "", fmt.Errorf("could not check the verified binary: %w", err)
	}
	if !now.Mode().IsRegular() || !os.SameFile(opened, now) {
		return "", errors.New("the file at that path is no longer the one that was verified")
	}
	return b.Path, nil
}
