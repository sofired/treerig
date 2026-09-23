package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/procid"
)

// judgeDelay is how long the second look waits, both for a contradiction
// (the flock is free, but the record's identity checks say its holder is
// still running) and for the marker rescan after a holder judged gone.
const judgeDelay = 1 * time.Second

// recoverPreviousHolder runs while this process already holds the flock: it
// judges any record left by a holder that never released, and cleans up a
// TypeScript-era leftover from before this lock was ported.
func (l *Lock) recoverPreviousHolder() error {
	rec, err := readRecord(l.recordPath())
	if err != nil {
		return l.refuse(fmt.Sprintf("its holder record %s could not be read (%s), so whether its holder is gone cannot be told", l.recordPath(), err))
	}
	if rec != nil && rec.ReleasedAt == nil {
		if err := l.judgeAndHandle(rec, false); err != nil {
			return err
		}
	}
	return l.recoverLegacyLeftover()
}

// judgeAndHandle ports step 4 of the design: judge, refuse on doubt, and
// catch a contradiction — the flock is free, yet the record's identity
// checks say its holder is still running.
func (l *Lock) judgeAndHandle(rec *Record, skipFileIdentity bool) error {
	self, err := l.opts.Table.Self()
	if err != nil {
		self = procid.Identity{}
	}
	hostname, _ := os.Hostname()

	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, skipFileIdentity)
	if err != nil {
		return err
	}

	if verdict.Kind == Alive {
		time.Sleep(judgeDelay)
		verdict, err = judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, skipFileIdentity)
		if err != nil {
			return err
		}
		if verdict.Kind == Alive {
			return clierr.Newf(
				"The %s lock was free, but its record names a holder that still appears to be running (%s). "+
					"Locking does not work on this filesystem; refusing to continue.", l.scope.Description, verdict.Reason)
		}
	}
	if verdict.Kind == Unverifiable {
		return l.refuse(verdict.Reason)
	}
	// Gone: judged dead. Still scan for anything it started before trusting
	// the lock is really free of its work.
	return l.scanSurvivors()
}

func (l *Lock) refuse(reason string) error {
	return clierr.Newf("Not taking the %s lock: %s.", l.scope.Description, reason)
}

// scanSurvivors is the marker scan step 4 describes: rescanning once after a
// second, so something just finishing is not mistaken for a survivor.
func (l *Lock) scanSurvivors() error {
	if l.scope.Markers.Project == "" && l.scope.Markers.WorktreeID == "" {
		return nil
	}
	survivors, err := l.markedSurvivors()
	if err != nil || len(survivors) == 0 {
		return err
	}
	time.Sleep(judgeDelay)
	survivors, err = l.markedSurvivors()
	if err != nil || len(survivors) == 0 {
		return err
	}
	return l.refuse(fmt.Sprintf(
		"its previous holder is gone, but %s it started %s still running:\n%s",
		pluralCount(len(survivors)), pluralIsAre(len(survivors)), listProcs(survivors)))
}

func (l *Lock) markedSurvivors() ([]procid.Proc, error) {
	scan, err := l.opts.Table.Scan()
	if err != nil {
		return nil, err
	}
	return procid.Marked(scan.Procs, l.scope.Markers), nil
}

func pluralCount(n int) string {
	if n == 1 {
		return "a process"
	}
	return fmt.Sprintf("%d processes", n)
}

func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// legacyRecord is the TypeScript build's holder-record shape, written
// directly into the target file rather than a sidecar.
type legacyRecord struct {
	PID                int     `json:"pid"`
	Host               string  `json:"host"`
	Command            string  `json:"command"`
	StartedAt          string  `json:"startedAt"`
	MachineID          string  `json:"machineId"`
	BootID             string  `json:"bootId"`
	PIDNS              string  `json:"pidNs"`
	StartTicks         *uint64 `json:"startTicks"`
	ProcessStartedAtMs *int64  `json:"processStartedAtMs"`
}

func readLegacyRecord(target string) (*Record, error) {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() == 0 || info.Size() > recordMaxBytes {
		return nil, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, nil
	}
	var raw legacyRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil
	}
	if raw.PID <= 0 || raw.Host == "" {
		return nil, nil
	}
	rec := &Record{
		PID: raw.PID, Host: raw.Host, Command: raw.Command,
		MachineID: raw.MachineID, BootID: raw.BootID, PIDNS: raw.PIDNS, StartTicks: raw.StartTicks,
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.StartedAt); err == nil {
		rec.StartedAt = t
	}
	if raw.ProcessStartedAtMs != nil {
		us := *raw.ProcessStartedAtMs * 1_000
		rec.StartUs = &us
	}
	return rec, nil
}

// recoverLegacyLeftover cleans up a pre-port `<target>.lock` directory and
// the holder record the old build wrote inside `<target>` itself — counted
// as "not released" the same way an unreleased sidecar record is, judged and
// scanned the same way, then removed while still holding the flock.
func (l *Lock) recoverLegacyLeftover() error {
	lockDir := l.scope.Target + ".lock"
	info, err := os.Lstat(lockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil // not what a TypeScript leftover looks like; leave it alone
	}
	legacy, err := readLegacyRecord(l.scope.Target)
	if err != nil {
		return err
	}
	if legacy == nil {
		return nil // nothing to judge; an unidentified directory is left alone
	}
	if err := l.judgeAndHandle(legacy, true); err != nil {
		return err
	}
	if err := os.Remove(lockDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(l.scope.Target, nil, 0o600)
}
