package commands

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/worktree"
)

// Dev is `dev` (#5): the CommandContext, the real shared router, and the
// real checkout index, all built from env, as for Up.
func Dev(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	routerSvc, err := realRouter(env, cctx)
	if err != nil {
		return err
	}
	return DevWithContext(ctx, cctx, routerSvc, bestEffortIndex{cctx})
}

// DevWithContext is `dev`, operating directly on a CommandContext: `up`, then
// the logs of the worktree's project followed until the session ends.
//
// It is UpWithContext, every check, shared service and hook of it, with the
// development-session lock held from the moment the operation lock is taken
// until the follower is gone, and the operation lock held only for `up`'s own
// work (docs/design/locks.md). While the session lasts, `worktree remove`
// refuses (it takes the development lock), a second `dev` refuses, and
// `up`, `down`, `run` and the other verbs that need only the operation lock
// work as usual. If `up` fails, so does `dev`, the same way, and no logs are
// followed.
//
// It never returns nil: the session ends by Ctrl-C (see followLogs), by the
// follower ending on its own, or by an error of `up`'s. Whether the follower
// ends when the worktree's containers go varies (see
// backend.Compose.Logs): where it keeps waiting, `treerig down` leaves the
// session idle, and `treerig up` from another terminal may make it show the new
// containers' logs.
func DevWithContext(ctx context.Context, cctx *CommandContext, routerSvc Router, index Index) error {
	_, err := UpWithContext(ctx, cctx, routerSvc, index, UpOptions{
		follow: func(ctx context.Context, ref worktree.Ref, host string) error {
			return followLogs(ctx, cctx, ref, host)
		},
	})
	return err
}

// ctrlC is the status a session ended by Ctrl-C exits with: 128 plus SIGINT,
// as a shell reports a command a SIGINT ended and as `run` does.
const ctrlC = 128 + int(syscall.SIGINT)

// containerPoll is how often a session checks that its worktree still has a
// container; a variable so a test need not wait out the real interval.
var containerPoll = 5 * time.Second

// sessionEnded is what a development session says when the user ended it,
// the same for every way there is: Ctrl-C at the terminal, or a signal sent to
// treerig itself. It adds that the services keep running, and how to stop them,
// unless the last look found none.
func sessionEnded(cctx *CommandContext, host string, sig syscall.Signal, noContainers bool) string {
	message := fmt.Sprintf("The development session of %s ended (%s).", host, clierr.SignalName(sig))
	if noContainers {
		return message
	}
	return message + fmt.Sprintf(" Its services are still running; \"%s down\" stops them.", cctx.CommandName)
}

// followLogs follows ref's logs until the session ends, and says how it did.
// It runs with only the development-session lock held.
//
// A Ctrl-C at a terminal goes to the follower's own foreground process
// group, not to treerig, so it arrives here as the follower ending by SIGINT
// (or exiting 130, as Compose does when it handles the interrupt itself):
// that is the session ending as asked, reported with sessionEnded's notice
// and exit status 130. A SIGINT, SIGTERM or SIGHUP that reaches treerig itself
// (a kill, or a Ctrl-C with no terminal) cancels ctx: the runner stops the
// follower's process group first, the same notice is printed, and the error
// returned is the cancellation, which ends the process by that signal, exit
// 128+n, once the lock is released.
//
// Compose's follower may or may not end when the containers do (see
// backend.Compose.Logs). One that keeps waiting leaves a session whose
// containers are all gone (a `down` from another terminal) idle, with one
// notice saying so. Any end of the follower other than the above is the
// session ending on its own, with a message and the follower's exit code, or
// 1 for exit 0.
func followLogs(ctx context.Context, cctx *CommandContext, ref worktree.Ref, host string) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	cctx.Logger.Info(fmt.Sprintf("Following the logs of %s. Press Ctrl-C to end the session.", host))
	runtime := cctx.Runtime(cctx.Loaded.Config.LabelNamespace())

	watchCtx, stopWatching := context.WithCancel(ctx)
	var watching sync.WaitGroup
	var noContainers atomic.Bool
	watching.Add(1)
	go func() {
		defer watching.Done()
		watchContainers(watchCtx, cctx, runtime, ref, host, &noContainers)
	}()
	result, err := runtime.Logs(ctx, ref)
	stopWatching()
	watching.Wait()

	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			cctx.Logger.Info(sessionEnded(cctx, host, cancelled.Signal, noContainers.Load()))
		}
		return err
	}
	// A follower ended by SIGINT reports 128+SIGINT too, so one test covers
	// both it and Compose handling the interrupt itself.
	if result.ExitCode == ctrlC {
		return clierr.Ended(sessionEnded(cctx, host, syscall.SIGINT, noContainers.Load()), ctrlC)
	}
	status := fmt.Sprintf("; \"%s status\" shows what is running.", cctx.CommandName)
	code := max(result.ExitCode, 1)
	switch {
	case result.Signal != 0:
		return clierr.WithCode(fmt.Sprintf("The log follower of %s was terminated by %s, so the development session is over%s",
			host, clierr.SignalName(result.Signal), status), code)
	case result.ExitCode != 0:
		return clierr.WithCode(fmt.Sprintf("The log follower of %s ended on its own (exit %d), so the development session is over%s",
			host, result.ExitCode, status), code)
	}
	return clierr.WithCode(fmt.Sprintf("The log follower of %s ended on its own (exit 0), usually because none of its containers is left running, so the development session is over%s", host, status), code)
}

// watchContainers tells the user, once each time it happens, that the
// worktree has no container left, since Compose's follower says nothing and
// the session would look hung. It polls at containerPoll until ctx ends; a
// probe that fails says nothing and is tried again. none records whether the
// last successful look found no container, for the session's closing notice.
// Cheap by design: one
// `compose ps` every few seconds, and nothing at all when the follower is
// gone.
func watchContainers(ctx context.Context, cctx *CommandContext, runtime *backend.Compose, ref worktree.Ref, host string, none *atomic.Bool) {
	ticker := time.NewTicker(containerPoll)
	defer ticker.Stop()
	told := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		has, err := runtime.HasContainers(ctx, ref)
		if err != nil {
			continue
		}
		none.Store(!has)
		switch {
		case has:
			told = false
		case !told:
			told = true
			cctx.Logger.Info(fmt.Sprintf("No container of %s is left, and \"%s worktree remove\" refuses until the session ends; "+
				"Ctrl-C ends the session, or run \"%s up\" in another terminal to start them again.", host, cctx.CommandName, cctx.CommandName))
		}
	}
}
