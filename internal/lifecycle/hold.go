package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
)

// Quiescer is the one thing Hold needs from a run.Runner: proof that every
// child it started has been reaped. run.Runner itself does not declare
// Quiesce (only osrun.Runner does), so this is its own small interface —
// the consumer's, not the implementer's — and osrun.Runner satisfies it
// structurally without either package naming the other.
type Quiescer interface {
	Quiesce(ctx context.Context) error
}

// Env is what Hold needs from the process running it, decoupled from
// internal/commands.Env so this package never has to import commands (which
// will come to depend on this one, not the other way round).
type Env struct {
	Logger logger.Logger
	Runner Quiescer
	// Invocation names this process to whoever waits on a lock it holds —
	// the command line as invoked, e.g. "berth up api".
	Invocation string
}

// Requests is which locks one Hold call needs. Lock order is fixed
// everywhere: operation, then development, then shared, then registry; a nil
// field is simply not requested. Router is taken alone — a router request
// never appears alongside a worktree's own locks, since the router is
// machine-wide rather than scoped to any one repository or control home.
//
// Shared is a repository's shared-services lock (docs/design/routing.md,
// decision 18). Nothing takes it and then a worktree's lock: a command that
// needs both holds the worktree's lock first, or takes the shared lock alone.
type Requests struct {
	Operation   *lock.Scope
	Development *lock.Scope
	Shared      *lock.Scope
	Registry    *lock.Scope
	Router      *lock.Scope
	// Timeout, when positive, replaces every requested lock's wait budget:
	// a caller that would rather skip a busy lock than wait for it (the
	// reconnect sweep) sets it. Zero keeps each scope's own.
	Timeout time.Duration
	// Quiet silences the periodic "waiting for the lock" notice, for a
	// caller whose Timeout is short and that reports a busy lock itself.
	Quiet bool
}

// ordered is r's requested scopes in the fixed acquisition order: operation,
// then development, then shared, then registry, then router, skipping
// whichever were not requested.
func (r Requests) ordered() []*lock.Scope {
	var scopes []*lock.Scope
	for _, s := range []*lock.Scope{r.Operation, r.Development, r.Shared, r.Registry, r.Router} {
		if s != nil {
			scopes = append(scopes, s)
		}
	}
	return scopes
}

// watchdogForTest overrides every lock Hold acquires with a short watchdog
// interval, for tests only; zero (its default) leaves lock.Options'
// withDefaults own 5 s default in place. Unexported, and reached only
// through SetWatchdogForTest, the way progress.heartbeat is reached only
// through progress.SetHeartbeatForTest.
var watchdogForTest time.Duration

// SetWatchdogForTest shortens every lock Hold acquires to interval for the
// life of a test, so a lost lock can be observed without waiting on the real
// 5 s default. It is not safe for tests that run in parallel with each
// other.
func SetWatchdogForTest(interval time.Duration) (restore func()) {
	previous := watchdogForTest
	watchdogForTest = interval
	return func() { watchdogForTest = previous }
}

// optionsFor is the lock.Options for scope: its timeout (and, for the
// registry, its retry interval) fixed by which of requests' fields it is.
func optionsFor(scope *lock.Scope, requests Requests, env Env) lock.Options {
	opts := lock.Options{Holder: env.Invocation, Logger: env.Logger, Watchdog: watchdogForTest}
	switch scope {
	case requests.Operation:
		opts.Timeout = lock.OperationTimeout
	case requests.Development:
		opts.Timeout = lock.DevelopmentTimeout
	case requests.Shared:
		opts.Timeout = lock.SharedTimeout
	case requests.Registry:
		opts.Timeout = lock.RegistryTimeout
		opts.Retry = lock.RegistryRetry
	case requests.Router:
		opts.Timeout = lock.RouterTimeout
	}
	if requests.Timeout > 0 {
		opts.Timeout = requests.Timeout
		opts.Retry = 0
	}
	if requests.Quiet {
		never := 24 * time.Hour // longer than any wait budget
		opts.NoticeDelay = &never
	}
	return opts
}

// Hold acquires the requested locks in order, runs action with a context
// that is cancelled if ctx itself ends or if any held lock is later lost,
// and on the way out proves the action's subprocesses quiesced before
// releasing the locks in reverse order. If quiescence cannot be proven, the
// locks are abandoned instead — never released while something they guarded
// might still be running — and the process is torn down: see Lifecycle.Exit.
//
// The action's own error is reported first; a release failure rides along as
// its Cause (if it had none) and always in Suppressed, so a lock that could
// not be released is never silent.
func (l *Lifecycle) Hold(ctx context.Context, env Env, requests Requests, action func(context.Context) error) error {
	return l.hold(ctx, env, requests, action)
}

// Nest is Hold for the locks that come after the ones a surrounding Hold
// already took: it acquires requests' scopes in the fixed order and hands
// them to action beside the outer ones, so a Docker child started inside
// inherits all of them, outermost first. It exists for exactly one nesting
// (docs/design/routing.md, decision 18): a worktree's operation lock, then
// the repository's shared lock, for a connect step. Requests may name only
// Shared or Registry, and only a kind that comes after every kind the
// surrounding Hold took (Hold records them on the context it hands its
// action), so no caller can invert the order; anything else is a bug and is
// refused.
func (l *Lifecycle) Nest(ctx context.Context, env Env, requests Requests, action func(context.Context) error) error {
	if requests.Operation != nil || requests.Development != nil || requests.Router != nil {
		return clierr.New("lifecycle: Nest takes only the shared and registry locks, which come after a worktree's; this is a bug in berth, please report it.")
	}
	held := heldRank(ctx)
	for rank, scope := range map[int]*lock.Scope{rankShared: requests.Shared, rankRegistry: requests.Registry} {
		if scope != nil && rank <= held {
			return clierr.New("lifecycle: Nest was asked for a lock that does not come after the ones already held, which would invert the lock order; this is a bug in berth, please report it.")
		}
	}
	return l.hold(ctx, env, requests, action)
}

// The order locks are taken in, as ranks: a lock may be taken inside a Hold
// only if its rank is greater than every rank that Hold took.
const (
	rankOperation = iota
	rankDevelopment
	rankShared
	rankRegistry
	rankRouter
)

type heldRankKey struct{}

// heldRank is the highest rank of the locks ctx's surrounding Hold took, or -1
// when there is none.
func heldRank(ctx context.Context) int {
	if rank, ok := ctx.Value(heldRankKey{}).(int); ok {
		return rank
	}
	return -1
}

// highestRank is the highest rank among the scopes r requests, or -1.
func (r Requests) highestRank() int {
	highest := -1
	for rank, scope := range []*lock.Scope{r.Operation, r.Development, r.Shared, r.Registry, r.Router} {
		if scope != nil {
			highest = max(highest, rank)
		}
	}
	return highest
}

// hold is Hold and Nest: acquire requests' scopes in order, appending them to
// whatever ctx already carries for inheritance, run action, prove its
// children quiesced, and release in reverse order.
func (l *Lifecycle) hold(ctx context.Context, env Env, requests Requests, action func(context.Context) error) error {
	scopes := requests.ordered()
	held := make([]*lock.Lock, 0, len(scopes))

	actionCtx := ctx
	for _, scope := range scopes {
		lk := lock.New(*scope, optionsFor(scope, requests, env))
		var err error
		actionCtx, err = lk.Acquire(actionCtx, env.Invocation)
		if err != nil {
			releaseAll(held)
			return err
		}
		held = append(held, lk)
	}

	// The operation lock first, at fd 3: lock.Inherited(ctx) is what
	// InheritLocks reads, so a Docker child started from inside action
	// actually gets the lock — without this, InheritLocks silently hands
	// the child nothing. A nested lock follows the outer ones.
	for _, lk := range held {
		actionCtx = lock.AddHeld(actionCtx, lk)
	}
	if highest := requests.highestRank(); highest >= 0 {
		actionCtx = context.WithValue(actionCtx, heldRankKey{}, max(highest, heldRank(ctx)))
	}

	actionErr := action(actionCtx)

	needsQuiesce := actionErr != nil || actionCtx.Err() != nil
	var quiesceErr error
	if needsQuiesce && env.Runner != nil {
		quiesceErr = env.Runner.Quiesce(context.Background())
	}

	if quiesceErr != nil {
		// Logged before exiting: the reason must be seen, since nothing
		// else will report it.
		message := fmt.Sprintf("Could not prove the interrupted operation stopped: %s", quiesceErr)
		if env.Logger != nil {
			env.Logger.Error(message)
		}
		abandonAll(held)
		l.os.Exit(1) // real code never returns from this
		return clierr.WithCode(message, 1)
	}

	var releaseErrs []error
	for i := len(held) - 1; i >= 0; i-- {
		if err := held[i].Release(); err != nil {
			releaseErrs = append(releaseErrs, err)
		}
	}

	// A lock the watchdog found lost during action was only closed above,
	// never marked released — its record may belong to whoever holds it
	// now — and Release itself reports no error for that case. Left
	// unchecked, an action that returned nil despite the loss (one that
	// does not itself watch ctx.Done()) would make this whole Hold call
	// report success. This checks explicitly, rather than leaving actionErr
	// alone to speak for it.
	for _, lk := range held {
		if lk.Lost() {
			releaseErrs = append(releaseErrs, lk.Err())
		}
	}
	return attach(actionErr, releaseErrs)
}

// releaseAll releases every lock in held, last acquired first, ignoring any
// error: used only when Hold itself is giving up before running action, so
// there is nothing yet for a release failure to be attached to.
func releaseAll(held []*lock.Lock) {
	for i := len(held) - 1; i >= 0; i-- {
		_ = held[i].Release()
	}
}

// abandonAll abandons every lock in held, last acquired first: their records
// stay unreleased, for the next acquirer to judge, because whatever they
// guarded may still be running.
func abandonAll(held []*lock.Lock) {
	for i := len(held) - 1; i >= 0; i-- {
		_ = held[i].Abandon()
	}
}

// attach makes primary the reported error, with every error in secondary
// riding along: as Cause when primary has none of its own, and always in
// Suppressed, so a lock that could not be released is never silent.
func attach(primary error, secondary []error) error {
	if primary == nil {
		if len(secondary) == 0 {
			return nil
		}
		primary, secondary = secondary[0], secondary[1:]
	}
	if len(secondary) == 0 {
		return primary
	}
	var ce *clierr.Error
	if !errors.As(primary, &ce) || ce.Message != primary.Error() {
		// Not our own mutable error type (or a wrapper whose text is more
		// than the refusal inside it): rebuilt around its text so Cause
		// and Suppressed still have somewhere to go; its own structure
		// (Unwrap chain) does not survive this.
		ce = &clierr.Error{Message: primary.Error(), ExitCode: clierr.Code(primary)}
	}
	if ce.Cause == nil {
		ce.Cause = secondary[0]
	}
	ce.Suppressed = append(ce.Suppressed, secondary...)
	return ce
}
