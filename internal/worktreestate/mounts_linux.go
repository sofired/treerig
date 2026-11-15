//go:build linux

package worktreestate

import (
	"fmt"
	"os"
)

// readMounts reads the mount table from /proc/self/mountinfo.
func readMounts() (Mounts, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("reading the mount table: %w", err)
	}
	return parseMountInfo(string(data))
}
