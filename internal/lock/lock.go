// Package lock is berth's only synchronization primitive: flock(2) on the
// lock target file itself, held by an open file description. The kernel
// frees it the moment its holder dies — even by SIGKILL — and leaves
// nothing stale, and it survives setsid, fork and exec for a child that
// inherited the descriptor. There is no stale window, no `.recovery` mutex,
// no `.lock` directory — only judgeHolder's proof that a silent holder is
// truly gone.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
)

// Options configures one Lock. Every field but Timeout has a sensible
// default; tests override Clock, Table, and the notice schedule to run fast
// and deterministic.
type Options struct {
	// Timeout bounds how long Acquire waits. Zero uses the same short
	// exponential backoff ScopedLock used with neither option set.
	Timeout time.Duration
	// Retry is the fixed poll interval once Timeout is set; zero picks a 250 ms ceiling.
	Retry time.Duration
	// NoticeDelay is how long to wait before the first "waiting for the lock"
	// notice; nil defaults to 2 s. A pointer because zero (notice at once) is
	// a real, test-only setting.
	NoticeDelay *time.Duration
	// NoticeInterval is how often the notice repeats; zero defaults to 30 s.
	NoticeInterval time.Duration
	// Watchdog is how often Lost is checked; zero defaults to 5 s. Negative disables it.
	Watchdog time.Duration
	// Holder is how this process was invoked ("berth up", say), recorded for whoever waits on it.
	Holder string
	Logger logger.Logger
	// Table identifies and inspects processes; nil defaults to procid.Platform().
	Table procid.Table
	// Clock is the wait's clock; tests use it to simulate a suspended machine.
	Clock func() time.Time
}

// withDefaults fills in every field New relies on having a usable value,
// leaving Timeout, Retry and NoticeDelay for waitPlan and noticeDelay to
// default on their own since zero is a meaningful setting for each.
func (o Options) withDefaults() Options {
	if o.NoticeInterval <= 0 {
		o.NoticeInterval = 30 * time.Second
	}
	if o.Watchdog == 0 {
		o.Watchdog = 5 * time.Second
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Table == nil {
		o.Table = procid.Platform()
	}
	if o.Logger == nil {
		o.Logger = logger.Silent{}
	}
	return o
}

// noticeDelay is how long Acquire waits before its first "waiting for the
// lock" notice: NoticeDelay if set, else 2s.
func (o Options) noticeDelay() time.Duration {
	if o.NoticeDelay != nil {
		return *o.NoticeDelay
	}
	return 2 * time.Second
}

// waitPlan is one of three schedules: a timeout polls at a fixed interval
// for its whole budget; an interval alone polls at that interval for twenty
// tries; neither is a short exponential backoff, about 4.6 s in all.
func (o Options) waitPlan() (budget, first, max time.Duration) {
	switch {
	case o.Timeout > 0:
		interval := o.Retry
		if interval <= 0 {
			interval = 250 * time.Millisecond
		}
		if interval > o.Timeout {
			interval = o.Timeout
		}
		return o.Timeout, interval, interval
	case o.Retry > 0:
		return 20 * o.Retry, o.Retry, o.Retry
	default:
		return 4600 * time.Millisecond, 50 * time.Millisecond, 250 * time.Millisecond
	}
}

// Lock is one scope's flock, configured but not yet acquired.
type Lock struct {
	scope Scope
	opts  Options

	checkFSOnce sync.Once
	checkFSErr  error

	mu      sync.Mutex
	file    *os.File
	fileID  FileID
	record  Record
	held    bool
	lost    bool
	lostErr error
	// refuseVerb is the verb refuse() and its callers use to name what this
	// Lock was trying to do — "taking" for every ordinary Lock, "clearing"
	// for the *Lock Clear builds to reuse the same judging and recovery
	// machinery without ever holding on as a new holder. Empty (every Lock
	// built through New for Acquire) defaults to "taking". Display only:
	// nothing branches on its value — see mentionUnverifiableFlag.
	refuseVerb string
	// mentionUnverifiableFlag says whether a refusal born of doubt should
	// suggest --holder-unverifiable. Clear sets it from
	// ClearOptions.SuggestUnverifiableFlag, true only for a worktree lock —
	// the one kind `worktree unlock --holder-unverifiable` can actually
	// clear; an ordinary Lock built for Acquire leaves it false, since
	// nothing it waits on has such a flag to offer.
	mentionUnverifiableFlag bool
	// watchdogStop tells the watchdog goroutine to end cleanly on an
	// ordinary Release or Abandon. It is deliberately not the action
	// context's own cancel func: stopping the watchdog must never itself
	// cancel the action, or a plain Abandon would SIGTERM whatever the
	// action is still running through an inherited lock.
	watchdogStop chan struct{}
	watchdogDone chan struct{}
}

// warnUnidentified says once per process that its locks cannot be recovered automatically.
var warnUnidentified sync.Once

// New configures a Lock over scope; it does nothing to the filesystem until Acquire.
func New(scope Scope, opts Options) *Lock {
	return &Lock{scope: scope, opts: opts.withDefaults()}
}

// recordPath is where the holder record sidecar for this lock's target lives.
func (l *Lock) recordPath() string { return l.scope.Target + ".holder" }

// ensureCheckedFS runs CheckFS against the target's directory once per Lock,
// creating the directory first if this is the first lock ever taken there.
// The result is cached so every later Acquire on the same Lock skips the
// self-test and reuses its verdict.
func (l *Lock) ensureCheckedFS() error {
	l.checkFSOnce.Do(func() {
		// The first lock of a new repository finds no directory yet; the
		// self-test needs one to open its probe in, as the target itself does.
		if err := os.MkdirAll(dirOf(l.scope.Target), 0o700); err != nil {
			l.checkFSErr = err
			return
		}
		l.checkFSErr = CheckFS(dirOf(l.scope.Target), l.scope.HomeEnvVar)
	})
	return l.checkFSErr
}

// Acquire takes the lock, waiting as Options.Timeout allows. It returns a
// context derived from ctx: cancelled if ctx itself ends, and also cancelled
// if the watchdog later finds the lock lost. ctx.Done() before or during the
// wait ends it with ctx's own cancellation cause; nothing is held then.
func (l *Lock) Acquire(ctx context.Context, command string) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, context.Cause(ctx)
	}
	if err := l.ensureCheckedFS(); err != nil {
		return ctx, err
	}

	file, fileID, err := openTarget(l.scope.Target, l.scope.Description)
	if err != nil {
		return ctx, err
	}

	budget, first, maxInterval := l.opts.waitPlan()
	started := l.opts.Clock()
	deadline := started.Add(budget)
	interval := first
	nextNotice := started.Add(l.opts.noticeDelay())

	for {
		flockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if flockErr == nil {
			match, serr := sameFile(file, l.scope.Target)
			if serr != nil {
				file.Close()
				return ctx, serr
			}
			if !match {
				// Berth never unlinks lock files, so this is a very tight
				// race with something else replacing the path; reopen and retry.
				file.Close()
				if now := l.opts.Clock(); !now.Before(deadline) {
					return ctx, l.giveUpError(now, fileID)
				}
				file, fileID, err = openTarget(l.scope.Target, l.scope.Description)
				if err != nil {
					return ctx, err
				}
				continue
			}
			l.mu.Lock()
			l.file, l.fileID = file, fileID
			l.mu.Unlock()
			return l.finishAcquire(ctx, command)
		}
		if !errors.Is(flockErr, unix.EWOULDBLOCK) && !errors.Is(flockErr, unix.EAGAIN) {
			file.Close()
			return ctx, flockErr
		}

		now := l.opts.Clock()
		if !now.Before(deadline) {
			file.Close()
			return ctx, l.giveUpError(now, fileID)
		}
		if !now.Before(nextNotice) {
			l.notice(fileID)
			nextNotice = l.opts.Clock().Add(l.opts.NoticeInterval)
		}
		sleep := interval
		if remaining := deadline.Sub(now); remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			file.Close()
			return ctx, context.Cause(ctx)
		case <-time.After(sleep):
		}
		if interval < maxInterval {
			interval *= 2
			if interval > maxInterval {
				interval = maxInterval
			}
		}
	}
}

// finishAcquire runs once the flock itself is ours: judging any previous
// holder that never released, recording this process as the new holder, and
// starting the watchdog.
func (l *Lock) finishAcquire(ctx context.Context, command string) (context.Context, error) {
	if err := l.recoverPreviousHolder(); err != nil {
		l.mu.Lock()
		file := l.file
		l.file = nil
		l.mu.Unlock()
		file.Close()
		return ctx, err
	}

	self, err := l.opts.Table.Self()
	if err != nil {
		self = procid.Identity{}
	}
	if self.MachineID == "" && self.BootID == "" {
		warnUnidentified.Do(func() {
			l.opts.Logger.Warn("This machine's identity could not be read (no machine ID and no boot ID), " +
				"so if this command dies holding a lock, nothing can later prove it gone and the lock will need clearing by hand.")
		})
	}
	hostname, _ := os.Hostname()
	rec := Record{
		PID: os.Getpid(), Host: hostname, Command: oneLineCommand(command, l.opts.Holder),
		StartedAt: l.opts.Clock(), File: l.fileID,
		MachineID: self.MachineID, BootID: self.BootID, PIDNS: self.PIDNS,
		StartTicks: self.StartTicks, StartUs: self.StartUs,
	}
	if err := writeRecordAtomic(l.recordPath(), rec); err != nil {
		l.mu.Lock()
		file := l.file
		l.file = nil
		l.mu.Unlock()
		file.Close()
		return ctx, clierr.Wrap(fmt.Sprintf(
			"could not record this process as the holder of the %s lock, so it was given back: %s",
			l.scope.Description, err), err)
	}

	l.mu.Lock()
	l.record = rec
	l.held = true
	l.mu.Unlock()

	actionCtx, cancel := context.WithCancelCause(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	l.mu.Lock()
	l.watchdogStop, l.watchdogDone = stop, done
	l.mu.Unlock()
	go l.watchdog(stop, done, cancel)
	return actionCtx, nil
}

// oneLineCommand is what gets written to the record as this holder's
// command: Options.Holder when the caller supplied one, else the raw
// invocation string.
func oneLineCommand(invocation, holder string) string {
	if holder != "" {
		return holder
	}
	return invocation
}

// Release writes releasedAt to the record, then closes the descriptor — the
// only thing that actually frees the flock; LOCK_UN would release it for
// every fd sharing the open file description, including a child's inherited
// one. If a descendant still holds it afterwards, it warns rather than waits.
//
// A lock the watchdog found lost is abandoned instead: its record may
// belong to whoever holds the lock now, and it is not this process's to mark.
func (l *Lock) Release() error {
	l.mu.Lock()
	file, held, rec := l.file, l.held, l.record
	l.file, l.held = nil, false
	l.mu.Unlock()
	if file == nil {
		return nil
	}
	// Read only once the watchdog has stopped, so a check it was in the
	// middle of cannot find the lock lost after this looked.
	l.stopWatchdog()
	if l.Lost() {
		return file.Close()
	}
	// The watchdog's own last tick can be arbitrarily out of date by the
	// time it actually stops — ownership can change in exactly that window,
	// with nothing left running to catch it. One synchronous check here, the
	// same one the watchdog itself makes, closes that gap: found lost, it is
	// recorded exactly as a watchdog-detected loss is, so a caller like
	// Hold, checking Lost() after release, still sees it.
	//
	// Whether the record below is still this process's to mark released is
	// judged separately from that: checkStillOurs also fails when only the
	// target's own file identity has changed — TestWaiterReopensAReplacedTarget
	// is exactly this, a target replaced while held, with nothing else ever
	// having raced in to claim the record — and there marking it released is
	// not just safe but needed, for whatever reopens the replaced target
	// next to recover cleanly. A record that no longer names this process at
	// all is different: it may already belong to whoever holds the lock now,
	// and it is not this process's to overwrite, exactly as the existing
	// Lost() check above already declines to.
	if err := l.checkStillOurs(); err != nil {
		l.mu.Lock()
		l.lost, l.lostErr = true, err
		l.mu.Unlock()
		if current, readErr := readRecord(l.recordPath()); readErr != nil || current == nil || current.PID != os.Getpid() {
			return file.Close()
		}
	}

	if held {
		now := l.opts.Clock()
		rec.ReleasedAt = &now
		if err := writeRecordAtomic(l.recordPath(), rec); err != nil {
			l.opts.Logger.Warn(fmt.Sprintf(
				"Could not note this process as having released the %s lock: %s", l.scope.Description, err))
		}
	}
	closeErr := file.Close()

	if held {
		if probe, err := os.OpenFile(l.scope.Target, os.O_RDWR, 0o600); err == nil {
			if flockErr := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); flockErr != nil {
				l.opts.Logger.Warn(fmt.Sprintf(
					"Processes started by this command still hold the %s lock.", l.scope.Description))
			}
			probe.Close()
		}
	}
	return closeErr
}

// Abandon closes the descriptor only, so the record stays unreleased for the
// next acquirer to judge.
func (l *Lock) Abandon() error {
	l.mu.Lock()
	file := l.file
	l.file, l.held = nil, false
	l.mu.Unlock()
	if file == nil {
		return nil
	}
	l.stopWatchdog()
	return file.Close()
}

// stopWatchdog ends the watchdog goroutine for an ordinary Release or
// Abandon. It must never cancel the action's own context — that is reserved
// for the watchdog itself, the moment it actually finds the lock lost.
func (l *Lock) stopWatchdog() {
	l.mu.Lock()
	stop, done := l.watchdogStop, l.watchdogDone
	l.watchdogStop, l.watchdogDone = nil, nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

// File is the held *os.File, for ExtraFiles.
func (l *Lock) File() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file
}

// Lost reports whether the watchdog found this lock no longer ours.
func (l *Lock) Lost() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// Err is why the lock was lost, if it was.
func (l *Lock) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lostErr
}

// Describe names this lock's scope and command, for a report — never for
// judging a holder from outside Acquire.
func (l *Lock) Describe() string { return l.scope.Description }

// watchdog checks every Options.Watchdog that the path's inode and the
// record are still ours; if not, it cancels the action's context. Fixes #22:
// a compromised lock now cancels in-flight work. stop ends the goroutine on
// an ordinary Release or Abandon, without touching cancel — only an actual
// loss, found right here, ever calls it. A disabled watchdog (tests only)
// still runs this goroutine, just to hold cancel until stop closes; it never
// fires on its own.
func (l *Lock) watchdog(stop <-chan struct{}, done chan<- struct{}, cancel context.CancelCauseFunc) {
	defer close(done)
	if l.opts.Watchdog <= 0 {
		<-stop
		return
	}
	ticker := time.NewTicker(l.opts.Watchdog)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := l.checkStillOurs(); err != nil {
				l.mu.Lock()
				l.lost, l.lostErr = true, err
				l.mu.Unlock()
				cancel(err)
				return
			}
		}
	}
}

// checkStillOurs is the watchdog's periodic proof that this process still
// holds the lock: the target file must still be the one it opened, by
// device and inode, and the holder record must still name this PID. Either
// failing means someone else may hold the lock now, so the watchdog must
// stop trusting it; a record that is merely unreadable is not evidence of
// that, since the file-identity check above already covers replacement.
func (l *Lock) checkStillOurs() error {
	var st syscall.Stat_t
	if err := syscall.Lstat(l.scope.Target, &st); err != nil {
		return clierr.Wrap(fmt.Sprintf("The %s lock file was removed while this command held it, so another process may hold it now; stopping.", l.scope.Description), err)
	}
	if uint64(st.Dev) != l.fileID.Dev || uint64(st.Ino) != l.fileID.Ino {
		return clierr.Newf("The %s lock file was replaced while this command held it, so another process may hold it now; stopping.", l.scope.Description)
	}
	rec, err := readRecord(l.recordPath())
	if err != nil {
		// Unreadable is not replaced: the file identity above already holds,
		// so a transient read error is no reason to stop a long operation.
		return nil
	}
	if rec == nil || rec.PID != os.Getpid() {
		return clierr.Newf("The %s lock's record no longer names this process, so another process may hold it now; stopping.", l.scope.Description)
	}
	return nil
}
