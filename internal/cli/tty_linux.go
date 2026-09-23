//go:build linux

package cli

import "golang.org/x/sys/unix"

// stdinIsTTY reports whether the process's real stdin (fd 0) is a terminal,
// the way Node's process.stdin.isTTY does — independent of whatever Reader
// Main was actually given, which is what commands.Env.StdinIsTTY serves the
// confirmation prompt with.
func stdinIsTTY() bool {
	_, err := unix.IoctlGetTermios(0, unix.TCGETS)
	return err == nil
}

// stderrIsTTY reports whether the real stderr (fd 2) is a terminal, which is
// where the confirmation prompt's colour goes.
func stderrIsTTY() bool {
	_, err := unix.IoctlGetTermios(2, unix.TCGETS)
	return err == nil
}
