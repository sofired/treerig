//go:build !linux && !darwin

package worktreestate

import "errors"

// readMounts has no mount table to read on this platform, so a directory's
// absence is never taken as evidence that it was deleted.
func readMounts() (Mounts, error) {
	return nil, errors.New("this platform has no mount table treerig reads")
}
