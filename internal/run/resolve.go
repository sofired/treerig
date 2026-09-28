package run

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ResolveExecutable finds command exactly as osrun's Runner does before ever
// spawning it: a command containing "/" is checked directly — it must
// exist, be a regular file (exec(2) itself refuses anything else — a FIFO,
// a directory, a device or a socket — with EACCES; a FIFO in particular is
// never opened at all here to find that out, since reading one blocks until
// a writer connects), and have its execute bit set — and a bare name is
// searched on PATH, read from env's own "PATH=" entry, or this process's
// real PATH when env is nil, exactly as a spawned child would see it —
// never this process's own PATH regardless, so a caller-supplied Env is
// honored for the lookup too. Exported so a caller with no process to spawn
// (doctor's hook-resolution check) predicts a Run's own 126/127 from the
// same single implementation, rather than a second guess of its own.
func ResolveExecutable(command string, env []string) (string, *StartError) {
	if strings.Contains(command, "/") {
		info, err := os.Stat(command)
		if err != nil {
			return "", &StartError{
				Command: command, Code: "ENOENT",
				Detail:   "does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).",
				ExitCode: 127,
			}
		}
		if !info.Mode().IsRegular() {
			return "", &StartError{Command: command, Code: "EACCES", Detail: "is not a regular file (EACCES).", ExitCode: 126}
		}
		if unix.Access(command, unix.X_OK) != nil {
			return "", &StartError{Command: command, Code: "EACCES", Detail: "is not executable (EACCES).", ExitCode: 126}
		}
		return command, nil
	}
	path := PathEnv(env)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, command)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if unix.Access(candidate, unix.X_OK) != nil {
			continue
		}
		return candidate, nil
	}
	return "", &StartError{Command: command, Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127}
}

// PathEnv reads PATH out of env, the environment a spawned command resolves
// against; a nil env means the command inherits this process's own, so PATH
// comes from the real environment instead.
func PathEnv(env []string) string {
	if env == nil {
		return os.Getenv("PATH")
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			return entry[len("PATH="):]
		}
	}
	return ""
}

// shebangLineLimit bounds how many bytes classifyFormat reads looking for a
// "#!" line's own interpreter: generously long for any real interpreter
// path and its argument, short enough that a large binary that merely
// starts with '#' is never read in full.
const shebangLineLimit = 256

// elfMagic and the Mach-O magics (32-bit, 64-bit, and fat/universal, each
// byte-order pair) are the native executable headers classifyFormat
// recognizes, alongside a "#!" interpreter line: together what exec(2)
// needs to run a file directly. A file with neither is what gives a real
// Run its ENOEXEC — osrun's own classifyExecError discovers that only
// because the kernel makes this exact check at exec(2) time; classifyFormat
// is that same check, made by reading the file instead, for a caller with
// no exec(2) to ask.
var (
	elfMagic      = [4]byte{0x7f, 'E', 'L', 'F'}
	machO32Magic  = [2][4]byte{{0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}}
	machO64Magic  = [2][4]byte{{0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}}
	machOFatMagic = [2][4]byte{{0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca}}
)

// isNativeMagic reports whether header is a recognized ELF or Mach-O header.
func isNativeMagic(header [4]byte) bool {
	if header == elfMagic {
		return true
	}
	for _, magic := range machO32Magic {
		if header == magic {
			return true
		}
	}
	for _, magic := range machO64Magic {
		if header == magic {
			return true
		}
	}
	for _, magic := range machOFatMagic {
		if header == magic {
			return true
		}
	}
	return false
}

// classifyFormat reports whether resolved — already confirmed to exist, be a
// regular file, and have its execute bit set (by ResolveExecutable for
// depth 0, by classifyShebang's own interpreter check for depth > 0) — is
// something exec(2) can run directly: a native ELF or Mach-O header, or a
// "#!" interpreter line. depth is which binary-handler attempt this is, 0
// for the hook's own script; any file at depth > execMaxDepth, and a "#!"
// line found at depth > shebangMaxDepth, is refused outright — never recursed into — with the *StartError a real
// exec(2) on this OS gives an interpreter chain that deep (see
// resolve_linux.go and resolve_darwin.go, and shebangDepthError's own
// callers below). dir is the working directory a relative interpreter path
// resolves against, passed straight through to classifyShebang.
//
// A file whose execute bit is set but that cannot be opened for reading —
// mode 0111, say — is reported runnable without inspection: execute-only is
// not a format problem, and there is nothing here to read that would prove
// otherwise.
func classifyFormat(resolved, dir string, depth int) (bool, *StartError) {
	// The kernel counts every binary-handler attempt, whatever the file
	// turns out to be, so a native binary one level too deep is refused
	// just like a script there; see execMaxDepth.
	if depth > execMaxDepth {
		return false, shebangDepthError(resolved)
	}
	f, err := os.Open(resolved)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return true, nil
		}
		return false, &StartError{Detail: fmt.Sprintf("could not be read to check its executable format: %s.", err), ExitCode: 126}
	}
	defer f.Close()

	buf := make([]byte, shebangLineLimit)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, &StartError{Detail: fmt.Sprintf("could not be read to check its executable format: %s.", err), ExitCode: 126}
	}
	header := buf[:n]

	if len(header) >= 4 {
		var magic [4]byte
		copy(magic[:], header[:4])
		if isNativeMagic(magic) {
			return true, nil
		}
	}
	if len(header) >= 2 && header[0] == '#' && header[1] == '!' {
		// Only Darwin reaches this: on Linux execMaxDepth equals
		// shebangMaxDepth, so the check at the top has already refused.
		if depth > shebangMaxDepth {
			return false, shebangDepthError(resolved)
		}
		return classifyShebang(header[2:], dir, depth)
	}
	return false, nil
}

// classifyShebang parses the rest of a "#!" line — bounded by
// shebangLineLimit, and possibly missing its own trailing newline if the
// line ran past that bound — for its interpreter: the kernel's own
// tokenizer (Linux's binfmt_script) splits on a space or tab alone, never
// on '\r', so a shebang line saved with CRLF endings glues a trailing '\r'
// onto the interpreter name itself whenever nothing separates them — the
// common single-interpreter "#!/bin/sh" form. The interpreter is checked
// exactly as the kernel would see it, '\r' included, so a CRLF shebang is
// predicted to fail precisely when it would for real; hadCR only chooses
// which wording explains why, and a display copy has it trimmed so the
// message never prints a raw carriage return.
//
// Once the interpreter itself is found, regular, and executable — the same
// Stat-only check as everywhere else here, so an interpreter that is a FIFO
// is refused exactly like any other candidate, never opened, and this can
// never hang — its own format is classified too, recursing into
// classifyFormat one depth deeper: exec(2) does not stop at "the
// interpreter exists", it has to run it, and a script naming another script
// as its interpreter is exactly what shebangMaxDepth bounds. An interpreter
// that turns out to be neither a script nor a native binary is refused here
// (its own ENOEXEC), since only this function still has its own name to put
// in that message.
//
// Anything after the interpreter (an argument to env, say) is not this
// file's concern; a shebang naming no interpreter at all is left runnable,
// matching classifyFormat's own "found a '#!' line" verdict.
func classifyShebang(rest []byte, dir string, depth int) (bool, *StartError) {
	if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	fields := bytes.FieldsFunc(rest, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return true, nil
	}
	token := fields[0]
	hadCR := bytes.HasSuffix(token, []byte("\r"))
	interpreter := string(token)
	display := strings.TrimSuffix(interpreter, "\r")

	path := interpreter
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		detail := fmt.Sprintf("has a #! interpreter `%s` that does not exist (ENOENT).", display)
		if hadCR {
			detail = fmt.Sprintf(
				"has a #! interpreter `%s` that does not exist — the file's line endings are CRLF, which glues a stray \\r onto the interpreter name (ENOENT).",
				display)
		}
		return false, &StartError{Code: "ENOENT", Detail: detail, ExitCode: 127}
	}
	if !info.Mode().IsRegular() || unix.Access(path, unix.X_OK) != nil {
		return false, &StartError{Code: "EACCES", Detail: fmt.Sprintf("has a #! interpreter `%s` that is not executable (EACCES).", display), ExitCode: 126}
	}
	runnable, startErr := classifyFormat(path, dir, depth+1)
	if startErr != nil {
		return false, startErr
	}
	if !runnable {
		return false, &StartError{Code: "ENOEXEC", Detail: fmt.Sprintf(
			"has a #! interpreter `%s` that is not a valid executable (ENOEXEC) — it needs a #! line or a native format.", display), ExitCode: 126}
	}
	return true, nil
}

// PredictStart is ResolveExecutable, plus classifyFormat for a found,
// executable file's own would-be ENOEXEC (or, when it is a script, its own
// interpreter's would-be 127/126 — recursively, should that interpreter be
// a script too), together the whole verdict a real Run would reach without
// ever spawning anything — what doctor's own hook-resolution check needs,
// since it never spawns the hook to find out for itself. dir is the working
// directory a relative "#!" interpreter path resolves against, exactly as
// it would for a real Run of command with that same working directory:
// execve(2) never searches PATH for an interpreter, only the exec'ing
// process's own current directory, like any other relative path it is
// handed.
func PredictStart(command, dir string, env []string) (string, *StartError) {
	resolved, startErr := ResolveExecutable(command, env)
	if startErr != nil {
		return "", startErr
	}
	runnable, startErr := classifyFormat(resolved, dir, 0)
	if startErr != nil {
		startErr.Command = command
		return "", startErr
	}
	if !runnable {
		return "", &StartError{Command: command, Code: "ENOEXEC", Detail: "is not a valid executable (ENOEXEC) — a script needs a #! line.", ExitCode: 126}
	}
	return resolved, nil
}
