//go:build darwin

package cli

import "golang.org/x/sys/unix"

// stdinIsTTY reports whether the process's real stdin (fd 0) is a terminal.
// SPIKE: written to match tty_linux.go's contract but not run on macOS in
// this spike; see the CLI agent's final report.
func stdinIsTTY() bool {
	_, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	return err == nil
}

// stderrIsTTY reports whether the real stderr (fd 2) is a terminal, which is
// where the confirmation prompt's colour goes.
func stderrIsTTY() bool {
	_, err := unix.IoctlGetTermios(2, unix.TIOCGETA)
	return err == nil
}
