// Package run is the seam every subprocess goes through. Injecting a Runner
// is what makes the rest of the codebase testable without spawning anything.
// The operating-system implementation lives in internal/run/osrun; a scripted
// fake for tests lives in internal/run/fakerun.
package run

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/sofired/treerig/internal/clierr"
)

// Output says where a child's stdout and stderr go.
type Output int

const (
	// Capture keeps both streams for the caller and shows nothing.
	Capture Output = iota
	// Tee shows stdout on this process's stdout and stderr on its stderr while
	// capturing both. No caller uses it since Compose's transcript moved to
	// Transcript (stderr, and only with -v); it stays as the one mode in which
	// a child's stdout is the command's own result, which hooks no longer are.
	Tee
	// TeeToStderr shows both streams on this process's stderr while capturing them, for a
	// caller whose own stdout is a report the child's output must not interleave with.
	TeeToStderr
	// Interactive gives the child its own process group in this process's
	// session; nothing is captured. When this process's stdin is a terminal
	// and it currently holds that terminal's foreground, the child gets the
	// foreground too — atomically, as part of starting it — and the run
	// answers Ctrl-C and Ctrl-Z with the terminal's own job control;
	// otherwise (piped, backgrounded, ssh without -t, CI) the child gets
	// only the process group. See internal/run/osrun for how.
	Interactive
)

// Transcript is where the transcript of a child whose own progress treerig
// re-tells in its own words goes: Docker Compose's, say. With -v it is shown on
// this process's stderr, as the child wrote it, and without it nothing is
// shown. Either way the output is captured, so a failure can quote it.
func Transcript(verbose bool) Output {
	if verbose {
		return TeeToStderr
	}
	return Capture
}

// OutputLimit is how much of each stream is captured before the child is stopped.
const OutputLimit = 100 * 1024 * 1024

// Spec is one subprocess to run.
type Spec struct {
	Command string
	Args    []string
	// Dir is the working directory; empty means this process's.
	Dir string
	// Env is the child's entire environment; nothing is merged in. Nil inherits this process's.
	Env []string
	// Input is written to stdin; nil gives the child no stdin at all.
	Input []byte
	// AllowFailure returns a non-zero exit as a Result instead of an error.
	AllowFailure bool
	Output       Output
	// Timeout stops the child after this long; zero means no limit.
	Timeout time.Duration
	// OutputLimit is how much of each stream is captured before the child is
	// stopped; zero means the package's OutputLimit. A caller that expects a
	// short answer sets a small one, so a runaway child is stopped early.
	OutputLimit int
	// KillAfter is how long a SIGTERMed child gets before SIGKILL; zero means 10 s.
	KillAfter time.Duration
	// Label names the child in a message about what it left running, as a
	// noun phrase ("apply command"), so the message never prints its
	// arguments or an absolute path. Empty uses the executable's base name
	// and "command".
	Label string
	// HideLine, when set, keeps a line of the child's output off the stream a
	// Tee shows it on; the line is still captured, so a failure reads it. It is
	// asked once per complete line, without the line break. Capture and
	// Interactive ignore it.
	HideLine func(line string) bool
	// Observe, when set, is called with each complete line the child writes on
	// either stream, except those HideLine hides, without the line break, as it
	// arrives and whatever Output says is shown; a last line without a break comes when the child ends.
	// It may be called from two goroutines at once. Interactive ignores it. It
	// is how a long phase learns what the child last said.
	Observe func(line string)
	// InheritLocks hands the locks held in the context to the child as extra
	// file descriptors, so the lock stays held while the child runs even if
	// this process dies. For docker and docker compose only.
	//
	// It does not combine with Output: Interactive, and Interactive wins:
	// the locks are not handed over. Inheriting lets a child outlive this
	// process still holding the lock, whereas an interactive child's whole
	// process group is drained (SIGTERM, then SIGKILL) before Run returns, so
	// the lock is released only once nothing the command started is left.
	InheritLocks bool
}

// Result is how a subprocess ended.
type Result struct {
	Stdout string
	Stderr string
	// ExitCode is the child's exit code; a child ended by a signal reports 128
	// plus the signal number, as a shell would, and a child stopped for a
	// timeout or for overflowing its output reports 1 with the flag set.
	ExitCode       int
	TimedOut       bool
	OutputOverflow bool
	// Signal is set when a signal ended the child rather than an exit; a
	// timeout is reported as TimedOut alone.
	Signal syscall.Signal
}

// Runner runs subprocesses. Cancelling the context stops the child: SIGTERM
// to its process group, then SIGKILL after Spec.KillAfter — except an
// interactive child, which is sent the signal this process itself received
// (SIGINT stays SIGINT) rather than always SIGTERM, and whose whole process
// group is drained (SIGTERM, then SIGKILL after KillAfter) once its leader
// is gone, before Run returns, whether or not this process was ever
// signalled.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
	// Which reports whether an executable regular file by that name is on PATH.
	Which(name string) bool
}

// StartError is a child that never ran: the executable is missing, not
// executable, or could not be spawned. Distinct from a failed exit, which is
// the command's own verdict. Exit codes follow the shell: 127 for "not
// found", 126 for "not executable".
type StartError struct {
	Command string
	// Code is the errno name, e.g. ENOENT; empty when there was none.
	Code string
	// Detail is the reason as a predicate — "is not executable (EACCES)." — for
	// callers that name the command themselves.
	Detail   string
	ExitCode int
}

// Error reports the command name and why it could not start.
func (e *StartError) Error() string {
	return fmt.Sprintf("`%s` could not be started: it %s", e.Command, e.Detail)
}

// ExitStatus is 126 or 127.
func (e *StartError) ExitStatus() int { return e.ExitCode }

// Failure is the error a rejecting Run returns for a command that ran and did
// not succeed: "Command failed (cmd args): reason", exiting 124 on a timeout.
func Failure(spec Spec, result Result) *clierr.Error {
	detail := Tail(firstNonBlank(result.Stderr, result.Stdout), FailureTailLines)
	var reason string
	switch {
	case result.TimedOut:
		reason = fmt.Sprintf("timed out after %d ms", spec.Timeout.Milliseconds())
	case result.OutputOverflow:
		limit := OutputLimit
		if spec.OutputLimit > 0 {
			limit = spec.OutputLimit
		}
		if limit >= 1024*1024 {
			reason = fmt.Sprintf("produced more than %d MB of output and was stopped", limit/(1024*1024))
		} else {
			reason = fmt.Sprintf("produced more than %d KB of output and was stopped", limit/1024)
		}
	case result.Signal != 0:
		reason = "terminated by " + clierr.SignalName(result.Signal)
	case detail != "":
		reason = detail
		detail = ""
	default:
		reason = fmt.Sprintf("exit %d", result.ExitCode)
	}
	// What the child said last is the evidence, whichever way it ended: a
	// start that timed out or was killed has nothing else to show.
	if detail != "" {
		reason += "\nLast output:\n" + detail
	}
	code := 1
	if result.TimedOut {
		code = 124
	}
	return clierr.WithCode(fmt.Sprintf("Command failed (%s %s): %s", spec.Command, strings.Join(shownArgs(spec), " "), reason), code)
}

// FailureTailLines is how many of the last lines of a child's output a
// failure quotes.
const FailureTailLines = 50

// firstNonBlank is the first of values with something in it.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Limits on what Tail quotes, so that a child that wrote one enormous line
// (or none at all, only bytes) cannot make a failure message enormous either.
const (
	tailMaxBytes     = 8 * 1024
	tailMaxLineBytes = 500
)

// Tail is the end of text, for a message: at most n lines, each cut to 500
// bytes, and at most 8 KiB in all, with a line saying how many lines came
// before when it had to leave some out.
func Tail(text string, n int) string {
	// Only the end can matter, so a huge capture is not even split.
	omitted := false
	if len(text) > 4*tailMaxBytes {
		text, omitted = strings.ToValidUTF8(text[len(text)-4*tailMaxBytes:], ""), true
	}
	text = strings.TrimSpace(text)
	// Nothing printable (NUL bytes from /dev/zero, say) is no evidence.
	if strings.IndexFunc(text, func(r rune) bool { return !unicode.IsSpace(r) && !unicode.IsControl(r) }) < 0 {
		return ""
	}
	lines := strings.Split(text, "\n")
	dropped := 0
	if len(lines) > n {
		dropped = len(lines) - n
		lines = lines[len(lines)-n:]
	}
	for i, line := range lines {
		if len(line) > tailMaxLineBytes {
			lines[i] = strings.ToValidUTF8(line[:tailMaxLineBytes], "") + "..."
		}
	}
	// Whole lines from the end, while they fit.
	size, keep := 0, len(lines)
	for keep > 0 && size+len(lines[keep-1])+1 <= tailMaxBytes {
		keep--
		size += len(lines[keep]) + 1
	}
	dropped += keep
	lines = lines[keep:]
	if dropped == 0 && !omitted {
		return strings.Join(lines, "\n")
	}
	if omitted && dropped == 0 {
		return "(earlier output left out)\n" + strings.Join(lines, "\n")
	}
	return fmt.Sprintf("(%d earlier lines left out)\n%s", dropped, strings.Join(lines, "\n"))
}

// shownArgs are the arguments a failure names. A caller that hardens Git
// puts `-c key=value` pairs ahead of everything else; they say nothing of what
// was asked, and a long message is cut short just where Git's own reason is, so
// they are left out.
func shownArgs(spec Spec) []string {
	args := spec.Args
	if spec.Command != "git" {
		return args
	}
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args
}

// Finish applies Spec.AllowFailure: a non-zero exit becomes a Failure unless allowed.
func Finish(spec Spec, result Result) (Result, error) {
	if !spec.AllowFailure && (result.ExitCode != 0 || result.TimedOut || result.OutputOverflow) {
		return result, Failure(spec, result)
	}
	return result, nil
}
