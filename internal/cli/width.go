package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const defaultHelpWidth = 80

// terminalWidth is commander's getOutHelpWidth/getErrHelpWidth: the
// stream's window size when it is a TTY, else the default 80 columns help.js
// itself falls back to. Only an *os.File can be a terminal; a writer used in
// a test (a buffer) never is, which is what keeps the golden corpus at a
// fixed 80 columns regardless of the terminal running the tests.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return defaultHelpWidth
	}
	size, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 {
		return defaultHelpWidth
	}
	return int(size.Col)
}
