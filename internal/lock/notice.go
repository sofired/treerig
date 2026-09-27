package lock

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/text"
)

// formatDuration is 4s, 12m 30s, 2h 5m, 3d 4h — what a person at a terminal
// wants. Ported unchanged from src/operation-lock.ts's formatDuration.
func formatDuration(d time.Duration) string {
	seconds := int64(math.Round(d.Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, seconds%60)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, minutes%60)
	}
	days := hours / 24
	return fmt.Sprintf("%dd %dh", days, hours%24)
}

// who names the holder the way ScopedLock's who() did: quoted like the
// command beside it, so prose in a hostname cannot close the clause and
// read as part of this tool's own sentence.
func (l *Lock) who(rec *Record, now time.Time) string {
	if rec == nil {
		return "An unidentified holder has it"
	}
	held := formatDuration(now.Sub(rec.StartedAt))
	return fmt.Sprintf("The holder, %s, has held it for %s", HolderLabel(rec), held)
}

// listProcs renders up to 10 of procs as one PID-and-command line each, with
// a "… and N more" line if there were more than that.
func listProcs(procs []procid.Proc) string {
	var b strings.Builder
	const shown = 10
	for i, p := range procs {
		if i >= shown {
			fmt.Fprintf(&b, "  … and %d more\n", len(procs)-shown)
			break
		}
		fmt.Fprintf(&b, "  PID %d: %s\n", p.PID, text.OneLine(strings.Join(p.Args, " "), 160))
	}
	return strings.TrimRight(b.String(), "\n")
}

// holdingProcs asks the process table who holds fileID's flock right now,
// for the notice and the final refusal to show alongside the record's own
// holder; a lookup error is treated as no processes found rather than
// surfaced, since this is advisory only.
func (l *Lock) holdingProcs(fileID FileID) []procid.Proc {
	procs, err := l.opts.Table.Holding(fileID.Dev, fileID.Ino)
	if err != nil {
		return nil
	}
	return procs
}

// advice is what a waiter should know, and — kept from before this lock was
// ported to flock(2), because a stopped holder still keeps the OS lock
// exactly as a running one does — what to do about a holder judged stopped.
// It names the holder and, on Linux, the processes still holding the lock,
// with kill -CONT/kill <pid> advice; it carries no heartbeat age ("last
// heartbeat"/"so it is alive" language) because flock(2) proves a holder
// alive or gone the instant it is probed, no staleness confirmation across
// two looks (nothing to confirm once the flock itself is the proof), and no
// `rmdir` removal advice (recovery is automatic: the next acquirer clears an
// unreleased lock on its own, never by hand).
func (l *Lock) advice(rec *Record, now time.Time, final bool, holding []procid.Proc) string {
	who := l.who(rec, now)
	stopped := false
	if rec != nil {
		if status, err := l.opts.Table.Inspect(rec.PID); err == nil && status.State == procid.Stopped {
			stopped = true
		}
	}
	extra := ""
	if len(holding) > 0 {
		extra = "\nProcesses holding it:\n" + listProcs(holding)
	}
	if stopped {
		action := fmt.Sprintf("resume it with `kill -CONT %d` and let it finish, or end it", rec.PID)
		if final {
			action += ", then retry"
		}
		return fmt.Sprintf("%s; it is stopped, not gone: %s.%s", who, action, extra)
	}
	action := "Wait for it to finish, or stop it"
	if rec != nil {
		action = fmt.Sprintf("Wait for it to finish, or stop it (`kill %d`)", rec.PID)
	}
	if final {
		action += ", then retry"
	}
	if rec == nil {
		return fmt.Sprintf("%s. %s.%s", who, action, extra)
	}
	return fmt.Sprintf("%s; it still holds the lock. %s.%s", who, action, extra)
}

// notice logs the periodic "waiting for the lock" message while Acquire
// waits, reading the current holder record and holding processes fresh each
// time it is called.
func (l *Lock) notice(fileID FileID) {
	rec, _ := readRecord(l.recordPath())
	now := l.opts.Clock()
	holding := l.holdingProcs(fileID)
	l.opts.Logger.Info(fmt.Sprintf("Waiting for the %s lock. %s", l.scope.Description, l.advice(rec, now, false, holding)))
}

// giveUpError is the final error Acquire returns once it stops waiting: the
// same holder-and-advice wording as notice, but phrased as a refusal rather
// than a status update.
func (l *Lock) giveUpError(now time.Time, fileID FileID) error {
	rec, _ := readRecord(l.recordPath())
	holding := l.holdingProcs(fileID)
	return clierr.Newf("Another process holds the %s lock. %s", l.scope.Description, l.advice(rec, now, true, holding))
}
