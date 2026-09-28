//go:build darwin

package run

import "fmt"

// shebangMaxDepth is 0 on Darwin: unlike Linux's binfmt_script, the script
// image activator (exec_shell_imgact, xnu's bsd/kern/kern_exec.c) never
// recurses — an interpreter named on a "#!" line must itself be a native
// Mach-O (or fat/universal) binary, the same rule Solaris' own script
// activator enforces. classifyFormat's own depth is 0 for the hook's own
// script, so this refuses at the very first interpreter indirection that
// turns out to be another script, rather than tolerating any chain at all.
const shebangMaxDepth = 0

// execMaxDepth is 1 on Darwin: the hook's own script, then its interpreter,
// which must be native. A script found there is already refused through
// shebangMaxDepth, so nothing deeper is ever reached.
const execMaxDepth = 1

// shebangDepthError is the StartError a real exec(2) on Darwin gives when a
// "#!" interpreter is itself a script rather than a native binary: ENOEXEC.
func shebangDepthError(interpreter string) *StartError {
	return &StartError{Code: "ENOEXEC", Detail: fmt.Sprintf(
		"has a #! interpreter `%s` that is itself a script, which macOS will not run (ENOEXEC).", interpreter), ExitCode: 126}
}
