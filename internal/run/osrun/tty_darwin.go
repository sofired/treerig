//go:build darwin

package osrun

import "golang.org/x/sys/unix"

// stdinIsTerminal reports whether this process's real stdin (fd 0) is a
// terminal — the same TIOCGETA probe internal/cli/tty_darwin.go uses for its
// own stdin check, kept as its own copy here since that package's helper is
// unexported and this one has a different job: deciding whether an
// interactive child gets the terminal's foreground, not whether to draw a
// prompt.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	return err == nil
}
