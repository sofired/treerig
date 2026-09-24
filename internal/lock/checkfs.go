package lock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
)

// CheckFS refuses a control home on a filesystem where flock cannot be
// trusted: a network filesystem may accept the call without actually
// coordinating with any other client, or not support it at all. It is called
// once per scope's directory, the first time that scope is used.
func CheckFS(dir string) error {
	if name, refused := unsupportedFSType(dir); refused {
		return clierr.Newf(
			"%s is on %s, where locks are not reliable. Point BERTH_HOME at a local directory.",
			dir, name,
		)
	}
	return selfTestFlock(dir)
}

// selfTestFlock proves flock actually excludes a second opener on this
// filesystem: two opens of the same fresh file, the second must see
// EWOULDBLOCK taking the lock the first already holds.
func selfTestFlock(dir string) error {
	probe, err := os.CreateTemp(dir, ".flock-check-*")
	if err != nil {
		return err
	}
	path := probe.Name()
	defer os.Remove(path)
	defer probe.Close()

	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer second.Close()

	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return clierr.Wrap(fmt.Sprintf("could not test locking on %s: %s", dir, err), err)
	}
	err = unix.Flock(int(second.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return clierr.Newf(
			"%s does not actually enforce flock (a second open of the same file was not blocked); "+
				"locks there would not be reliable. Point BERTH_HOME at a local directory.", dir,
		)
	}
	if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
		return clierr.Wrap(fmt.Sprintf("could not test locking on %s: %s", dir, err), err)
	}
	return nil
}
