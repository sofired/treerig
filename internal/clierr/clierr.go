// Package clierr is the error a command reports to the user and exits with,
// rather than a crash.
//
// Anything reaching the top of the CLI that does not carry an exit status
// (see [Exiter]) is unexpected: it is reported with a hint to set
// BERTH_DEBUG, and exits 1.
package clierr

import (
	"errors"
	"fmt"
	"syscall"
)

// Exiter is an expected failure: one the user is told about as such, with
// the exit code the process should end with.
type Exiter interface {
	error
	ExitStatus() int
}

// Error is a failure the user is told about, with the exit code to use. A
// failure that hid others — a hook that failed while its lock could not be
// released, say — carries them in Cause and Suppressed so all are reported.
type Error struct {
	Message    string
	ExitCode   int
	Cause      error
	Suppressed []error
	// Notice marks an end that is not a failure: a session the user ended.
	// The top level reports the message as information, not as an error,
	// and the process still exits with ExitCode.
	Notice bool
}

// Error is the message to show the user.
func (e *Error) Error() string { return e.Message }

// ExitStatus is the exit code this failure calls for.
func (e *Error) ExitStatus() int { return e.ExitCode }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// New is an expected failure that exits 1.
func New(message string) *Error { return &Error{Message: message, ExitCode: 1} }

// Newf is New with formatting.
func Newf(format string, args ...any) *Error { return New(fmt.Sprintf(format, args...)) }

// WithCode is an expected failure with its own exit code.
func WithCode(message string, code int) *Error { return &Error{Message: message, ExitCode: code} }

// Ended is a normal end that still carries an exit status, such as a
// development session the user stopped with Ctrl-C: its message is reported
// as information rather than as an error.
func Ended(message string, code int) *Error {
	return &Error{Message: message, ExitCode: code, Notice: true}
}

// Wrap is an expected failure caused by another error.
func Wrap(message string, cause error) *Error {
	return &Error{Message: message, ExitCode: 1, Cause: cause}
}

// Cancelled is the error an operation fails with when a signal interrupted it.
type Cancelled struct {
	Signal syscall.Signal
}

// Error names the signal that cancelled the operation.
func (c *Cancelled) Error() string {
	return fmt.Sprintf("Operation cancelled by %s.", SignalName(c.Signal))
}

// ExitStatus of a cancelled operation is 1; the process then dies by the signal itself.
func (c *Cancelled) ExitStatus() int { return 1 }

// SignalName is the conventional name, SIGINT rather than "interrupt".
func SignalName(sig syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM", syscall.SIGABRT: "SIGABRT",
		syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGUSR1: "SIGUSR1",
		syscall.SIGUSR2: "SIGUSR2", syscall.SIGALRM: "SIGALRM",
	}
	if name, ok := names[sig]; ok {
		return name
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// Code is the exit code an error calls for: its own for an Exiter, 1 for anything else.
func Code(err error) int {
	var exiter Exiter
	if errors.As(err, &exiter) {
		return exiter.ExitStatus()
	}
	return 1
}

// Expected reports whether err is a failure the user is told about as such,
// rather than a crash that deserves the BERTH_DEBUG hint.
func Expected(err error) bool {
	var exiter Exiter
	return errors.As(err, &exiter)
}
