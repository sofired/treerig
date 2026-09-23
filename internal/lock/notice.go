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
	where := fmt.Sprintf("PID %d on %s", rec.PID, quoteOneLine(rec.Host))
	identity := where
	if rec.Command != "" {
		identity = fmt.Sprintf("%s (%s)", quoteOneLine(rec.Command), where)
	}
	return fmt.Sprintf("The holder, %s, has held it for %s", identity, held)
}

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

func (l *Lock) holdingProcs(fileID FileID) []procid.Proc {
	procs, err := l.opts.Table.Holding(fileID.Dev, fileID.Ino)
	if err != nil {
		return nil
	}
	return procs
}

// advice is what a waiter should know, and — kept from the heartbeat design,
// because a stopped holder still keeps the OS lock exactly as a running one
// does — what to do about a holder judged stopped. Wording changes from
// src/operation-lock.ts's advice() are accepted difference 3 in
// docs/decisions/0001-port-to-go.md: there is
// no more "last heartbeat"/"so it is alive" language (flock itself proves
// it), no more staleness confirmation across two looks, and no more `rmdir`
// removal advice (recovery is automatic; a lock is never removed by hand).
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

func (l *Lock) notice(fileID FileID) {
	rec, _ := readRecord(l.recordPath())
	now := l.opts.Clock()
	holding := l.holdingProcs(fileID)
	l.opts.Logger.Info(fmt.Sprintf("Waiting for the %s lock. %s", l.scope.Description, l.advice(rec, now, false, holding)))
}

func (l *Lock) giveUpError(now time.Time, fileID FileID) error {
	rec, _ := readRecord(l.recordPath())
	holding := l.holdingProcs(fileID)
	return clierr.Newf("Another process holds the %s lock. %s", l.scope.Description, l.advice(rec, now, true, holding))
}
