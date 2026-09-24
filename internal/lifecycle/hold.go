package lifecycle

import (
	"context"
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
// everywhere: operation, then development, then registry; a nil field is
// simply not requested.
type Requests struct {
	Operation   *lock.Scope
	Development *lock.Scope
	Registry    *lock.Scope
}

// ordered is r's requested scopes in the fixed acquisition order: operation,
// then development, then registry, skipping whichever were not requested.
func (r Requests) ordered() []*lock.Scope {
	var scopes []*lock.Scope
	for _, s := range []*lock.Scope{r.Operation, r.Development, r.Registry} {
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
	case requests.Registry:
		opts.Timeout = lock.RegistryTimeout
		opts.Retry = lock.RegistryRetry
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
// its Cause (if it had none) and always in Suppressed, as
// src/signal-lifecycle.ts:95-104 attached a lock-release failure.
func (l *Lifecycle) Hold(ctx context.Context, env Env, requests Requests, action func(context.Context) error) error {
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
	// the child nothing.
	if len(held) > 0 {
		actionCtx = lock.WithHeld(actionCtx, held...)
	}

	actionErr := action(actionCtx)

	needsQuiesce := actionErr != nil || actionCtx.Err() != nil
	var quiesceErr error
	if needsQuiesce && env.Runner != nil {
		quiesceErr = env.Runner.Quiesce(context.Background())
	}

	if quiesceErr != nil {
		// As src/commands/context.ts's realProcess.forceExit logged before
		// exiting: the reason must be seen, since nothing else will report it.
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
	// report success. This checks explicitly, the way
	// src/operation-lock.ts's assertHealthy did, rather than leaving
	// actionErr alone to speak for it.
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
	ce, ok := primary.(*clierr.Error)
	if !ok {
		// Not our own mutable error type: rebuilt around its text so Cause
		// and Suppressed still have somewhere to go; its own structure
		// (Unwrap chain) does not survive this, which is the accepted
		// difference from src/signal-lifecycle.ts, where every thrown value
		// is a real Error and is mutated in place instead.
		ce = &clierr.Error{Message: primary.Error(), ExitCode: clierr.Code(primary)}
	}
	if ce.Cause == nil {
		ce.Cause = secondary[0]
	}
	ce.Suppressed = append(ce.Suppressed, secondary...)
	return ce
}
