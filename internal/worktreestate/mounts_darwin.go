//go:build darwin

package worktreestate

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// readMounts reads the mount table with getfsstat(2), the call getmntinfo(3)
// wraps: the first call counts the mounted filesystems, the second lists them
// into a table with room to spare, because a filesystem can be mounted between
// the two calls. A table that comes back full may have been cut short, so it is
// read again, once, from a count taken afresh; one that is still full is an
// error, not a table with a mount missing.
func readMounts() (Mounts, error) {
	for attempt := 0; attempt < 2; attempt++ {
		count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
		if err != nil {
			return nil, fmt.Errorf("reading the mount table: %w", err)
		}
		table := make([]unix.Statfs_t, count+8)
		listed, err := unix.Getfsstat(table, unix.MNT_NOWAIT)
		if err != nil {
			return nil, fmt.Errorf("reading the mount table: %w", err)
		}
		if listed >= len(table) {
			continue
		}
		mounts := Mounts{}
		for _, entry := range table[:min(listed, len(table))] {
			mounts[filepath.Clean(unix.ByteSliceToString(entry.Mntonname[:]))] = unix.ByteSliceToString(entry.Fstypename[:])
		}
		return mounts, nil
	}
	return nil, errors.New("reading the mount table: it kept changing")
}
