package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/text"
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
		if _, err := l.judgeAndHandle(rec, false); err != nil {
			return err
		}
	}
	_, _, _, err = l.recoverLegacyLeftover(false)
	return err
}

// judgeAndHandle ports step 4 of the design: judge, refuse on doubt, and
// catch a contradiction — the flock is free, yet the record's identity
// checks say its holder is still running. allowUnverifiable is false for
// every ordinary Lock (recoverPreviousHolder never offers it); Clear passes
// through whatever --holder-unverifiable said. Returns the verdict it
// settled on, once every check — including the marker scan — has passed.
func (l *Lock) judgeAndHandle(rec *Record, allowUnverifiable bool) (Verdict, error) {
	self, err := l.opts.Table.Self()
	if err != nil {
		self = procid.Identity{}
	}
	hostname, _ := os.Hostname()

	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, false)
	if err != nil {
		return Verdict{}, err
	}

	if verdict.Kind == Alive {
		time.Sleep(judgeDelay)
		verdict, err = judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, false)
		if err != nil {
			return Verdict{}, err
		}
		if verdict.Kind == Alive {
			return Verdict{}, l.aliveContradiction(verdict.Reason)
		}
	}
	if verdict.Kind == Unverifiable && !allowUnverifiable {
		return Verdict{}, l.refuse(l.withUnverifiableAdvice(verdict.Reason))
	}
	// Gone, or an unverifiable verdict Clear was told to accept: still scan
	// for anything it started before trusting the lock is really free of its work.
	if err := l.scanSurvivors(); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// withUnverifiableAdvice appends the --holder-unverifiable suggestion only
// when mentionUnverifiableFlag says this Lock stands in for `worktree
// unlock` on a worktree scope: an ordinary acquirer (`up`, say), and Clear
// on the registry or router lock, have no such flag to offer.
func (l *Lock) withUnverifiableAdvice(reason string) string {
	if !l.mentionUnverifiableFlag {
		return reason
	}
	return reason + ". If you have made sure it, and every command it started on this machine, is gone, run this again with --holder-unverifiable"
}

// judgeLegacyHolder judges a TypeScript-era holder. Unlike a holder of this
// build's own lock, one found alive is no contradiction: the TypeScript build
// never takes the flock, so a free flock says nothing about it. It is simply
// running, and this build cannot wait on its lock.
func (l *Lock) judgeLegacyHolder(rec *Record, allowUnverifiable bool) (Verdict, error) {
	self, err := l.opts.Table.Self()
	if err != nil {
		self = procid.Identity{}
	}
	hostname, _ := os.Hostname()
	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, true)
	if err != nil {
		return Verdict{}, err
	}
	switch verdict.Kind {
	case Alive:
		return Verdict{}, l.legacyHolderAlive(verdict.Reason)
	case Unverifiable:
		if !allowUnverifiable {
			return Verdict{}, l.refuse(l.withUnverifiableAdvice(verdict.Reason))
		}
	}
	if err := l.scanSurvivors(); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

func (l *Lock) refuseVerbOrDefault() string {
	if l.refuseVerb != "" {
		return l.refuseVerb
	}
	return "taking"
}

func (l *Lock) refuse(reason string) error {
	return clierr.Newf("Not %s the %s lock: %s.", l.refuseVerbOrDefault(), l.scope.Description, reason)
}

// aliveContradiction is the refusal judgeAndHandle and judgeForClear both
// give when a second look still says the record's holder is running even
// though the flock itself is free: the lock design's own soundness
// assumption failed (a filesystem flock(2) does not actually exclude on,
// say), so this refuses outright rather than guessing further. Shared so the
// Acquire and Clear paths cannot drift apart on its wording.
func (l *Lock) aliveContradiction(reason string) error {
	return clierr.Newf(
		"The %s lock was free, but its record names a holder that still appears to be running (%s). "+
			"Locking does not work on this filesystem; refusing to continue.", l.scope.Description, reason)
}

// legacyHolderAlive is the refusal judgeLegacyHolder and judgeLegacyForClear
// both give when a TypeScript-era leftover names a holder still running:
// this build's own lock cannot see a TypeScript build's, so it can only wait
// it out. Shared so the Acquire and Clear paths cannot drift apart on its wording.
func (l *Lock) legacyHolderAlive(reason string) error {
	return clierr.Newf("Not %s the %s lock: a TypeScript build of %s holds it, and %s. "+
		"This build cannot wait on that build's locks, so run this again once that command is done.",
		l.refuseVerbOrDefault(), l.scope.Description, brand.Name, reason)
}

// legacyLockDirWithoutHolder is the refusal recoverLegacyLeftover and
// judgeLegacyForClear both give when a TypeScript-era lock directory exists
// but names no holder yet: a TypeScript build may be mid-way through taking
// it, and the two builds' locks cannot see each other to tell. Shared so the
// Acquire and Clear paths cannot drift apart on its wording.
func (l *Lock) legacyLockDirWithoutHolder(lockDir string) error {
	return clierr.Newf("Not %s the %s lock: %s exists but names no holder, so a TypeScript build of %s may be taking it right now. "+
		"If none is running, remove it with `rmdir %s` and run this again.",
		l.refuseVerbOrDefault(), l.scope.Description, text.Printable(lockDir), brand.Name, text.Printable(text.ShellQuote(lockDir)))
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
// scanned the same way, then removed while still holding the flock. Reports
// whether it found and cleared anything, and the verdict lines Clear reports
// as proof; recoverPreviousHolder ignores both, since it is not reporting a
// proof to anyone.
func (l *Lock) recoverLegacyLeftover(allowUnverifiable bool) (cleared bool, verified, unverified []string, err error) {
	lockDir := l.scope.Target + ".lock"
	info, statErr := os.Lstat(lockDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil, nil, nil
		}
		return false, nil, nil, statErr
	}
	if !info.IsDir() {
		return false, nil, nil, nil // not what a TypeScript leftover looks like; leave it alone
	}
	legacy, readErr := readLegacyRecord(l.scope.Target)
	if readErr != nil {
		return false, nil, nil, readErr
	}
	if legacy == nil {
		// A TypeScript berth takes its lock directory first and writes its
		// record a moment later; one that died in between leaves the same
		// sight. Taking the lock now could run beside a live one, and the
		// two builds' locks cannot see each other, so refuse either way.
		return false, nil, nil, l.legacyLockDirWithoutHolder(lockDir)
	}
	verdict, err := l.judgeLegacyHolder(legacy, allowUnverifiable)
	if err != nil {
		return false, nil, nil, err
	}
	if verdict.Kind == Unverifiable {
		unverified = append(unverified, verdict.Reason)
	} else {
		verified = append(verified, verdict.Reason)
	}
	if err := os.Remove(lockDir); err != nil && !os.IsNotExist(err) {
		return false, nil, nil, err
	}
	if err := os.WriteFile(l.scope.Target, nil, 0o600); err != nil {
		return false, nil, nil, err
	}
	return true, verified, unverified, nil
}

/* --------------------------------------------------------------- Clear --- */

// ClearOptions configures Clear.
type ClearOptions struct {
	// Table identifies and inspects processes; nil defaults to procid.Platform().
	Table procid.Table
	// AllowUnverifiable accepts a holder this machine cannot check (another
	// machine or PID namespace, or no record) instead of refusing —
	// `worktree unlock --holder-unverifiable`.
	AllowUnverifiable bool
	// Clock is Clear's own clock, for tests; nil defaults to time.Now.
	Clock func() time.Time
	// SuggestUnverifiableFlag says whether a refusal born of doubt should
	// suggest --holder-unverifiable — true only for a worktree lock, which
	// `worktree unlock --holder-unverifiable` can actually clear; the
	// registry and router locks have no such flag of their own (doctor's
	// Locks section instead offers hand removal for those two).
	SuggestUnverifiableFlag bool
}

func (o ClearOptions) withDefaults() ClearOptions {
	if o.Table == nil {
		o.Table = procid.Platform()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return o
}

// ScopeResult is what Clear found and did for one of its scopes.
type ScopeResult struct {
	Scope Scope
	// Cleared is true when this scope had something to clear: an unreleased
	// record, a TypeScript-era leftover, or both.
	Cleared bool
	// Verified is what was proven gone.
	Verified []string
	// Unverified is what was accepted on the word of --holder-unverifiable.
	Unverified []string
}

// ClearResult is what Clear found and did, across every scope it was given.
type ClearResult struct {
	Scopes []ScopeResult
	// Proof is the one line proven once every scope's holder judged clean
	// and no process any of them started is still running — "no command
	// started for this worktree is still running on this machine", or its
	// hidepid-accepted wording. "" when nothing was cleared, since there is
	// nothing to have proven.
	Proof string
}

// clearSleep waits for d, or ends early with ctx's own cancellation cause if
// ctx ends first first — the package's convention (see lifecycle.Lifecycle's
// own root context) for a wait that must not outlast a Ctrl-C.
func clearSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// judgeForClear judges rec's holder for a batched Clear: the same
// contradiction check judgeAndHandle makes for an ordinary Acquire (the
// flock is free, yet the record's identity checks say its holder is still
// running), and the same refusal on doubt unless allowUnverifiable — but no
// survivor scan of its own. Clear makes one combined scan for every scope
// together, once every one of them has judged clean, in scanSurvivorsAcross.
func (l *Lock) judgeForClear(ctx context.Context, rec *Record, allowUnverifiable bool) (Verdict, error) {
	self, err := l.opts.Table.Self()
	if err != nil {
		self = procid.Identity{}
	}
	hostname, _ := os.Hostname()

	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, false)
	if err != nil {
		return Verdict{}, err
	}
	if verdict.Kind == Alive {
		if err := clearSleep(ctx, judgeDelay); err != nil {
			return Verdict{}, err
		}
		verdict, err = judgeHolder(rec, l.fileID, self, hostname, l.opts.Table, false)
		if err != nil {
			return Verdict{}, err
		}
		if verdict.Kind == Alive {
			return Verdict{}, l.aliveContradiction(verdict.Reason)
		}
	}
	if verdict.Kind == Unverifiable && !allowUnverifiable {
		return Verdict{}, l.refuse(l.withUnverifiableAdvice(verdict.Reason))
	}
	return verdict, nil
}

// judgeLegacyForClear is recoverLegacyLeftover's own judging, split the same
// way judgeForClear splits judgeAndHandle: it reports what it found and
// judged, but neither scans for survivors nor removes anything — Clear does
// both only once every scope in the batch has judged clean. ok is false when
// there was no TypeScript-era leftover to judge at all.
func (l *Lock) judgeLegacyForClear(allowUnverifiable bool) (legacy *Record, verdict Verdict, ok bool, err error) {
	lockDir := l.scope.Target + ".lock"
	info, statErr := os.Lstat(lockDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, Verdict{}, false, nil
		}
		return nil, Verdict{}, false, statErr
	}
	if !info.IsDir() {
		return nil, Verdict{}, false, nil // not what a TypeScript leftover looks like; leave it alone
	}
	legacy, readErr := readLegacyRecord(l.scope.Target)
	if readErr != nil {
		return nil, Verdict{}, false, readErr
	}
	if legacy == nil {
		// A TypeScript berth takes its lock directory first and writes its
		// record a moment later; one that died in between leaves the same
		// sight. Taking the lock now could run beside a live one, and the
		// two builds' locks cannot see each other, so refuse either way.
		return nil, Verdict{}, false, l.legacyLockDirWithoutHolder(lockDir)
	}
	self, selfErr := l.opts.Table.Self()
	if selfErr != nil {
		self = procid.Identity{}
	}
	hostname, _ := os.Hostname()
	verdict, err = judgeHolder(legacy, l.fileID, self, hostname, l.opts.Table, true)
	if err != nil {
		return nil, Verdict{}, false, err
	}
	switch verdict.Kind {
	case Alive:
		return nil, Verdict{}, false, l.legacyHolderAlive(verdict.Reason)
	case Unverifiable:
		if !allowUnverifiable {
			return nil, Verdict{}, false, l.refuse(l.withUnverifiableAdvice(verdict.Reason))
		}
	}
	return legacy, verdict, true, nil
}

// heldRefusal is the refusal Clear and ProbeHeld give for a scope whose
// flock is held: named after its holder, and, on Linux, the processes still
// holding it — the same voice as an ordinary Lock's own giveUpError.
func (l *Lock) heldRefusal(fileID FileID) error {
	rec, _ := readRecord(l.recordPath())
	holding := l.holdingProcs(fileID)
	return clierr.Newf("The %s lock is held, so there is nothing to unlock. %s",
		l.scope.Description, l.advice(rec, l.opts.Clock(), true, holding))
}

// clearScope is one scope Clear is holding the flock of, judged but not yet committed.
type clearScope struct {
	scope         Scope
	file          *os.File
	fileID        FileID
	l             *Lock
	rec           *Record
	recVerdict    Verdict
	legacy        *Record
	legacyVerdict Verdict
}

// Clear is the core of `worktree unlock`: given every stale lock a target
// might have (a worktree's operation and development scopes), it probes
// each one's flock in turn, refusing at once — nothing committed anywhere —
// if any is held; otherwise it holds every one of them through judging every
// holder, one combined survivor scan, and finally committing each scope,
// so nothing else can reclaim a scope while another is still being judged.
//
// Judging never commits: only once every scope in the batch — every record,
// every TypeScript-era leftover, and the one shared survivor scan — has
// judged clean does Clear mark anything released, exactly as
// src/operation-lock.ts's clearStaleWorktreeLocks verifies every stale lock
// together before clearing any (tests/unit/operation-lock.test.ts:155,
// "preserves a stale operation lock when its recovery guard fails"). A scope
// with nothing unreleased and no leftover simply reports Cleared false.
//
// A failure during commit itself — a write that fails after an earlier
// scope's own commit already succeeded — still returns every ScopeResult
// decided so far, rather than discarding what was actually cleared.
func Clear(ctx context.Context, scopes []Scope, opts ClearOptions) (ClearResult, error) {
	opts = opts.withDefaults()

	var held []*clearScope
	defer func() {
		for _, h := range held {
			h.file.Close()
		}
	}()

	for _, scope := range scopes {
		file, err := os.OpenFile(scope.Target, os.O_RDWR, 0o600)
		if err != nil {
			if os.IsNotExist(err) {
				continue // never taken; nothing to hold, judge or clear
			}
			return ClearResult{}, err
		}
		fileID, err := fstatID(file)
		if err != nil {
			file.Close()
			return ClearResult{}, err
		}
		l := New(scope, Options{Table: opts.Table, Clock: opts.Clock})
		l.fileID = fileID
		l.refuseVerb = "clearing"
		l.mentionUnverifiableFlag = opts.SuggestUnverifiableFlag

		if flockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); flockErr != nil {
			if !errors.Is(flockErr, unix.EWOULDBLOCK) && !errors.Is(flockErr, unix.EAGAIN) {
				file.Close()
				return ClearResult{}, flockErr
			}
			refusal := l.heldRefusal(fileID)
			file.Close()
			return ClearResult{}, refusal
		}
		held = append(held, &clearScope{scope: scope, file: file, fileID: fileID, l: l})
	}
	if len(held) == 0 {
		return ClearResult{}, nil
	}

	// Phase 1: judge every scope's record and TypeScript-era leftover.
	// Nothing is committed here, so a refusal on any one leaves every
	// scope — including this one — exactly as it was.
	for _, h := range held {
		rec, err := readRecord(h.l.recordPath())
		if err != nil {
			return ClearResult{}, h.l.refuse(fmt.Sprintf(
				"its holder record %s could not be read (%s), so whether its holder is gone cannot be told", h.l.recordPath(), err))
		}
		if rec != nil && rec.ReleasedAt == nil {
			verdict, err := h.l.judgeForClear(ctx, rec, opts.AllowUnverifiable)
			if err != nil {
				return ClearResult{}, err
			}
			h.rec, h.recVerdict = rec, verdict
		}
		legacy, legacyVerdict, ok, err := h.l.judgeLegacyForClear(opts.AllowUnverifiable)
		if err != nil {
			return ClearResult{}, err
		}
		if ok {
			h.legacy, h.legacyVerdict = legacy, legacyVerdict
		}
	}

	// Phase 1.5: one shared survivor scan, still before any commit — but only
	// when some scope has a record or leftover to clear. With nothing to
	// clear, a survivor or hidden process would refuse a lock that is
	// already released, just as recoverPreviousHolder never scans for one.
	proof := ""
	for _, h := range held {
		if h.rec != nil || h.legacy != nil {
			var err error
			if proof, err = scanSurvivorsAcross(ctx, held, opts); err != nil {
				return ClearResult{}, err
			}
			break
		}
	}

	// Phase 2: commit. Still holding every flock, so nothing else can
	// reclaim a scope between one commit and the next.
	result := ClearResult{Scopes: make([]ScopeResult, 0, len(held))}
	anyCleared := false
	for _, h := range held {
		sr := ScopeResult{Scope: h.scope}
		if h.rec != nil {
			if h.recVerdict.Kind == Unverifiable {
				sr.Unverified = append(sr.Unverified, h.recVerdict.Reason)
			} else {
				sr.Verified = append(sr.Verified, h.recVerdict.Reason)
			}
			now := opts.Clock()
			rec := *h.rec
			rec.ReleasedAt = &now
			if err := writeRecordAtomic(h.l.recordPath(), rec); err != nil {
				result.Scopes = append(result.Scopes, sr)
				return result, clierr.Wrap(fmt.Sprintf(
					"Could not mark the %s lock's record released: %s", h.scope.Description, err), err)
			}
			sr.Cleared = true
		}
		if h.legacy != nil {
			if h.legacyVerdict.Kind == Unverifiable {
				sr.Unverified = append(sr.Unverified, h.legacyVerdict.Reason)
			} else {
				sr.Verified = append(sr.Verified, h.legacyVerdict.Reason)
			}
			lockDir := h.scope.Target + ".lock"
			if err := os.Remove(lockDir); err != nil && !os.IsNotExist(err) {
				result.Scopes = append(result.Scopes, sr)
				return result, err
			}
			if err := os.WriteFile(h.scope.Target, nil, 0o600); err != nil {
				result.Scopes = append(result.Scopes, sr)
				return result, err
			}
			sr.Cleared = true
		}
		if sr.Cleared {
			anyCleared = true
		}
		result.Scopes = append(result.Scopes, sr)
	}
	if anyCleared {
		result.Proof = proof
	}
	return result, nil
}

// scanSurvivorsAcross is the one combined process-table scan
// src/lock-recovery.ts's verify() makes for a whole worktree's stale locks:
// once, using whichever held scope's markers are set (every scope Clear is
// ever given for one worktree shares them), rescanning after judgeDelay so
// something just finishing is not mistaken for a survivor. Refuses (nothing
// committed) if it finds one, or if other users' processes are hidden from
// this one and opts.AllowUnverifiable was not given. Returns the proof line
// Clear reports once every scope has actually cleared.
func scanSurvivorsAcross(ctx context.Context, held []*clearScope, opts ClearOptions) (proof string, err error) {
	var markers procid.Markers
	for _, h := range held {
		if h.scope.Markers.Project != "" || h.scope.Markers.WorktreeID != "" {
			markers = h.scope.Markers
			break
		}
	}
	if markers.Project == "" && markers.WorktreeID == "" {
		return "no command started for this worktree is still running on this machine", nil
	}

	scan, err := opts.Table.Scan()
	if err != nil {
		return "", err
	}
	survivors := procid.Marked(scan.Procs, markers)
	unreadable, othersHidden := scan.Unreadable, scan.OthersHidden
	if len(survivors) > 0 {
		if err := clearSleep(ctx, judgeDelay); err != nil {
			return "", err
		}
		scan, err = opts.Table.Scan()
		if err != nil {
			return "", err
		}
		survivors = procid.Marked(scan.Procs, markers)
		unreadable, othersHidden = scan.Unreadable, scan.OthersHidden
	}
	if len(survivors) > 0 {
		return "", clierr.Newf("Not clearing any lock: %s started for this worktree %s still running:\n%s\nLet them finish or stop them, then run this again.",
			pluralCount(len(survivors)), pluralIsAre(len(survivors)), listProcs(survivors))
	}

	unread := ""
	if unreadable > 0 {
		unread = fmt.Sprintf(" (%d of your processes could not be read)", unreadable)
	}
	if !othersHidden {
		return "no command started for this worktree is still running on this machine" + unread, nil
	}
	reason := "other users' processes are hidden from you (/proc mounted with hidepid, say), so a command the holder ran " +
		"as another user — through sudo, say — could not be looked for"
	if !opts.AllowUnverifiable {
		return "", clierr.Newf("Not clearing any lock: %s. If you have made sure none is running, run this again with --holder-unverifiable.", reason)
	}
	return "none of your own processes started for this worktree is still running" + unread, nil
}

// ProbeHeld is a non-blocking, read-only probe of scope's flock: whether it
// is currently held, and, if so, the same refusal Clear itself would give
// for it. `worktree unlock` uses it to check every one of a worktree's locks
// before clearing any of them, so a lock found active after another has
// already been cleared can never happen; table drives the holder-naming
// (nil defaults to procid.Platform()).
func ProbeHeld(ctx context.Context, scope Scope, table procid.Table) (held bool, refusal error, err error) {
	if err := ctx.Err(); err != nil {
		return false, nil, context.Cause(ctx)
	}
	file, err := os.OpenFile(scope.Target, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	defer file.Close()

	fileID, err := fstatID(file)
	if err != nil {
		return false, nil, err
	}
	l := New(scope, Options{Table: table})

	if flockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); flockErr != nil {
		if !errors.Is(flockErr, unix.EWOULDBLOCK) && !errors.Is(flockErr, unix.EAGAIN) {
			return false, nil, flockErr
		}
		return true, l.heldRefusal(fileID), nil
	}
	return false, nil, nil
}
