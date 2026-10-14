package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/procid"
)

// judgeDelay is how long the second look waits, both for a contradiction
// (the flock is free, but the record's identity checks say its holder is
// still running) and for the marker rescan after a holder judged gone.
const judgeDelay = 1 * time.Second

// recoverPreviousHolder runs while this process already holds the flock: it
// judges any record left by a holder that never released.
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
	return nil
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

	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table)
	if err != nil {
		return Verdict{}, err
	}

	if verdict.Kind == Alive {
		time.Sleep(judgeDelay)
		verdict, err = judgeHolder(rec, l.fileID, self, hostname, l.opts.Table)
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

// refuseVerbOrDefault names the verb a refusal reports itself as failing to
// do: refuseVerb when one was set (Clear sets "clearing"), "taking" for an
// ordinary Lock.
func (l *Lock) refuseVerbOrDefault() string {
	if l.refuseVerb != "" {
		return l.refuseVerb
	}
	return "taking"
}

// refusalPrefix is the "Not <verb> the <scope> lock:" opening shared by the
// refusals of Acquire's recovery path.
func (l *Lock) refusalPrefix() string {
	return fmt.Sprintf("Not %s the %s lock:", l.refuseVerbOrDefault(), l.scope.Description)
}

// refuse builds the standard "Not <verb> the <scope> lock: <reason>." error
// for a reason that is a plain clause; a refusal whose reason ends in a list
// builds its own text instead.
func (l *Lock) refuse(reason string) error {
	return clierr.Newf("%s %s.", l.refusalPrefix(), reason)
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
	// Not l.refuse: the reason ends in a list, which cannot take refuse's
	// trailing period, so the advice follows on a line of its own.
	return clierr.Newf("%s its previous holder is gone, but %s it started %s still running:\n%s\n%s",
		l.refusalPrefix(), pluralCount(len(survivors)), pluralIsAre(len(survivors)),
		listProcs(survivors), survivorsAdvice(len(survivors)))
}

// survivorsAdvice is the sentence that follows a list of n surviving
// processes in a refusal, on a line of its own so that no punctuation is
// read as part of the last command line.
func survivorsAdvice(n int) string {
	if n == 1 {
		return "Let it finish or stop it, then run this again."
	}
	return "Let them finish or stop them, then run this again."
}

// markedSurvivors scans the process table and returns the processes carrying
// this scope's markers, the single scan scanSurvivors takes twice.
func (l *Lock) markedSurvivors() ([]procid.Proc, error) {
	scan, err := l.opts.Table.Scan()
	if err != nil {
		return nil, err
	}
	return procid.Marked(scan.Procs, l.scope.Markers), nil
}

// pluralCount renders n as "a process" for one, or "N processes" otherwise.
func pluralCount(n int) string {
	if n == 1 {
		return "a process"
	}
	return fmt.Sprintf("%d processes", n)
}

// pluralIsAre renders the verb to match pluralCount's noun: "is" for one,
// "are" otherwise.
func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
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
	// suggest --holder-unverifiable: true for every scope `worktree unlock`
	// can be pointed at, which by now is all four kinds (a worktree's own
	// locks, and, with --registry, --router or --shared, the registry, router
	// and a repository's shared-services lock — #21, #123).
	SuggestUnverifiableFlag bool
}

// withDefaults fills in o's zero-valued fields: procid.Platform() for Table,
// time.Now for Clock.
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
	// Cleared is true when this scope had an unreleased record to clear.
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
	// and no process any of them started is still running — "no command it
	// started is still running on this machine", or its hidepid-accepted
	// wording — or, for a scope with no markers of its own (the registry
	// lock: #21), that there was nothing of the kind to check. "" when
	// nothing was cleared, since there is nothing to have proven.
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

	verdict, err := judgeHolder(rec, l.fileID, self, hostname, l.opts.Table)
	if err != nil {
		return Verdict{}, err
	}
	if verdict.Kind == Alive {
		if err := clearSleep(ctx, judgeDelay); err != nil {
			return Verdict{}, err
		}
		verdict, err = judgeHolder(rec, l.fileID, self, hostname, l.opts.Table)
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
	scope      Scope
	file       *os.File
	fileID     FileID
	l          *Lock
	rec        *Record
	recVerdict Verdict
}

// Clear is the core of `worktree unlock`: given every stale lock a target
// might have (a worktree's operation and development scopes, or, with
// --registry or --router, the one machine-wide scope named — #21), it
// probes each one's flock in turn, refusing at once — nothing committed
// anywhere — if any is held; otherwise it holds every one of them through
// judging every holder, one combined survivor scan, and finally committing
// each scope, so nothing else can reclaim a scope while another is still
// being judged.
//
// Judging never commits: only once every scope in the batch — every record,
// and the one shared survivor scan — has judged clean does Clear mark
// anything released, so a stale lock is preserved whenever any scope's
// recovery guard fails. A scope with nothing unreleased simply reports
// Cleared false.
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
			if !os.IsNotExist(err) {
				return ClearResult{}, err
			}
			continue // never taken; nothing to hold, judge or clear
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

	// Phase 1: judge every scope's record. Nothing is committed here, so a
	// refusal on any one leaves every scope — including this one — exactly
	// as it was.
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
	}

	// Phase 1.5: one shared survivor scan, still before any commit — but only
	// when some scope has a record to clear. With nothing to clear, a
	// survivor or hidden process would refuse a lock that is already
	// released, just as recoverPreviousHolder never scans for one.
	proof := ""
	for _, h := range held {
		if h.rec != nil {
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

// scanSurvivorsAcross is the one combined process-table scan for a whole
// worktree's stale locks: once, using whichever held scope's markers are set
// (every scope Clear is
// ever given for one worktree, or the router lock, shares them), rescanning
// after judgeDelay so something just finishing is not mistaken for a
// survivor. Refuses (nothing committed) if it finds one, or if other users'
// processes are hidden from this one and opts.AllowUnverifiable was not
// given. Returns the proof line Clear reports once every scope has actually
// cleared. A scope with no markers at all — the registry lock, which
// guards only brief writes with no process of its own to mark (#21) —
// skips the scan outright and says so, rather than claiming a check that
// never ran.
func scanSurvivorsAcross(ctx context.Context, held []*clearScope, opts ClearOptions) (proof string, err error) {
	var markers procid.Markers
	for _, h := range held {
		if h.scope.Markers.Project != "" || h.scope.Markers.WorktreeID != "" {
			markers = h.scope.Markers
			break
		}
	}
	if markers.Project == "" && markers.WorktreeID == "" {
		return "it holds no process markers of its own, so there is nothing further to check", nil
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
		return "", clierr.Newf("Not clearing any lock: %s it started %s still running:\n%s\n%s",
			pluralCount(len(survivors)), pluralIsAre(len(survivors)), listProcs(survivors), survivorsAdvice(len(survivors)))
	}

	unread := ""
	if unreadable > 0 {
		unread = fmt.Sprintf(" (%d of your processes could not be read)", unreadable)
	}
	if !othersHidden {
		return "no command it started is still running on this machine" + unread, nil
	}
	reason := "other users' processes are hidden from you (/proc mounted with hidepid, say), so a command the holder ran " +
		"as another user — through sudo, say — could not be looked for"
	if !opts.AllowUnverifiable {
		return "", clierr.Newf("Not clearing any lock: %s. If you have made sure none is running, run this again with --holder-unverifiable.", reason)
	}
	return "none of your own processes it started is still running" + unread, nil
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
