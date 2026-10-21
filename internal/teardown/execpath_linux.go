package teardown

import (
	"errors"
	"fmt"
)

// ExecPath is the path to hand execve so that the program that runs is the
// file this Binary verified: on Linux the descriptor's own /proc entry, which
// the kernel resolves to the open file itself, so a rename or replacement of
// the stored path after the hash cannot change what runs. The descriptor is
// close-on-exec, which is fine for a binary (a script would not survive it, and
// a pinned treerig is not one). Keep the Binary open until the exec call
// returns.
func (b *Binary) ExecPath() (string, error) {
	if b == nil || b.file == nil {
		return "", errors.New("the verified binary is closed")
	}
	path := fmt.Sprintf("/proc/self/fd/%d", b.file.Fd())
	return path, nil
}
