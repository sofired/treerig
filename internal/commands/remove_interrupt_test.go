package commands_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
)

// interruptAt wraps a Runner so that the nth Docker call whose arguments
// contain match is where a signal arrives: cancel is called with the
// cancellation. The call then fails with it, unanswered, as a runner stopped
// by the signal does, unless proceed is set, in which case it is answered as
// usual and the command carries on with a cancelled context.
type interruptAt struct {
	run.Runner
	match   string
	nth     int
	cancel  context.CancelCauseFunc
	signal  syscall.Signal
	proceed bool
	// onInterrupt, when set, runs at the moment the signal arrives.
	onInterrupt func()
	seen        int
}

// Run delegates every call, interrupting the one the signal lands on.
func (r *interruptAt) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if strings.Contains(" "+strings.Join(spec.Args, " ")+" ", r.match) {
		r.seen++
		if r.seen == r.nth {
			cause := &clierr.Cancelled{Signal: r.signal}
			if r.onInterrupt != nil {
				r.onInterrupt()
			}
			r.cancel(cause)
			if !r.proceed {
				return run.Result{}, cause
			}
		}
	}
	return r.Runner.Run(ctx, spec)
}

// interruption describes one removal of feature-login that a signal
// interrupts: where, and with what around it.
type interruption struct {
	// signal defaults to SIGINT.
	signal syscall.Signal
	// match and nth pick the Docker call the signal lands on.
	match string
	nth   int
	// services are the worktree's services; extraVolumes more volumes it owns.
	services     []string
	extraVolumes int
	// proceed lets the call the signal lands on succeed.
	proceed bool
	// onInterrupt runs as the signal arrives, given the worktree's paths.
	onInterrupt func(layout.Worktree)
	// options are the removal's options; Yes is always set.
	options commands.RemoveOptions
	// commandName defaults to "treerig".
	commandName string
	// others registers a second worktree, feature-other, in a checkout of its own.
	others bool
}

// interrupted is what an interrupted removal returned and left.
type interrupted struct {
	err    error
	result *commands.RemoveResult
	lines  []string
	state  registry.WorktreeInitialization
	paths  layout.Worktree
	// targetID is the UUID of the worktree the removal targeted.
	targetID string
}

// remove runs the interruption's removal and reports what it left.
func (i interruption) remove(t *testing.T) interrupted {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	ref := w.Register("", acme, "feature-login", brand.LabelNamespace, i.services...)
	target, slug := ref, "feature-login"
	if i.others {
		target, slug = w.Register(w.Worktree("other", "feature/other"), acme, "feature-other", brand.LabelNamespace, i.services...), "feature-other"
	}
	for range i.extraVolumes {
		w.Docker.Add("volume", commandstest.Ownership(target, brand.LabelNamespace))
	}
	tc := w.Context("", acme, nil)
	name := i.commandName
	if name == "" {
		name = "treerig"
	}
	tc.CommandName = name
	sig := i.signal
	if sig == 0 {
		sig = syscall.SIGINT
	}
	paths := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	probe := &interruptAt{Runner: tc.Runner, match: i.match, nth: i.nth, cancel: cancel, signal: sig, proceed: i.proceed}
	if i.onInterrupt != nil {
		probe.onInterrupt = func() { i.onInterrupt(paths) }
	}
	tc.Runner = probe

	options := i.options
	options.Yes = true
	if options.Identifier == "." {
		// A path form needs the directory the card records, and a working directory to read from.
		w.WriteIndexCard(ref.RepositoryID, ref.WorktreeID, "acme", acme, "feature-login", "feature-login."+acme, w.Root, "feature/login")
		tc.Cwd = w.Root
	}
	result, err := commands.RemoveWithContext(ctx, tc.CommandContext, &commands.Env{CommandName: name}, options)
	lines, _ := tc.Logger.Snapshot()
	state, stateErr := tc.Registry.Store(target.RepositoryID).Initialization(target.WorktreeID, slug)
	if stateErr != nil && !i.proceed {
		t.Fatal(stateErr)
	}
	return interrupted{err: err, result: result, lines: lines, state: state, paths: paths, targetID: target.WorktreeID}
}

// interruptedLines is the lines of the notice an interrupted removal prints.
func interruptedLines(lines []string) []string {
	var found []string
	for _, line := range lines {
		if strings.Contains(line, "Interrupted (") {
			found = append(found, line)
		}
	}
	return found
}

// wantNotice fails t unless lines hold exactly the one interruption notice want.
func wantNotice(t *testing.T, lines []string, want string) {
	t.Helper()
	if got := interruptedLines(lines); !slices.Equal(got, []string{"info: " + want}) {
		t.Fatalf("notice = %q\nwant     %q", got, "info: "+want)
	}
}

func TestWorktreeRemoveInterrupted(t *testing.T) {
	const host = "feature-login.acme.localhost"
	const both = "'treerig worktree remove' finishes the removal, 'treerig up' starts it again and runs the seed hook again."

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		sig := sig
		t.Run("says what went, that the worktree is pending, and what to run next, on "+clierr.SignalName(sig), func(t *testing.T) {
			// Two volumes are reaped in order; the signal lands on the second.
			out := interruption{signal: sig, match: " volume rm ", nth: 2, services: []string{"api", "mail"}, extraVolumes: 1}.remove(t)
			var cancelled *clierr.Cancelled
			if !errors.As(out.err, &cancelled) || cancelled.Signal != sig {
				t.Fatalf("err = %v, want the cancellation, for the process to die by", out.err)
			}
			if out.state != registry.Pending {
				t.Fatalf("initialization = %v, want pending", out.state)
			}
			wantNotice(t, out.lines, "Interrupted ("+clierr.SignalName(sig)+") while removing "+host+". "+
				"Removed so far (at least): 2 containers, 1 volume. It is still registered, now pending: "+both)
		})
	}

	t.Run("says nothing was confirmed removed when the signal lands on the first resource", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1, extraVolumes: 1}.remove(t)
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far: nothing confirmed yet. "+
			"It is still registered, now pending: "+both)
	})

	t.Run("counts what compose down took when the signal lands before the first reap", func(t *testing.T) {
		// The second look for connected shared services is the one just after
		// the registration is marked pending.
		out := interruption{match: "label=dev.treerig.shared=true", nth: 2, services: []string{"api", "mail"}}.remove(t)
		if out.state != registry.Pending {
			t.Fatalf("initialization = %v, want pending", out.state)
		}
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far (at least): 2 containers. "+
			"It is still registered, now pending: "+both)
	})

	t.Run("counts the home directory when the signal lands after it went", func(t *testing.T) {
		// The signal lands on the last reap, which still succeeds; the home
		// goes, and releasing the worktree's ports then meets the cancelled
		// context, so the removal stops with the registration still there.
		out := interruption{match: " volume rm ", nth: 1, proceed: true}.remove(t)
		if out.err == nil {
			t.Fatal("the removal finished despite the cancelled context; releasing its ports should have stopped it")
		}
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far (at least): 1 volume, home directory. "+
			"It is still registered, now pending: "+both)
		if exists(t, out.paths.Home) {
			t.Fatal("the home survived")
		}
	})

	t.Run("names the command as it was run, and a target given as a slug by its UUID", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1, commandName: "bt", options: commands.RemoveOptions{Identifier: "feature-login", Domain: acme}}.remove(t)
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far: nothing confirmed yet. "+
			"It is still registered, now pending: 'bt worktree remove "+out.targetID+"' finishes the removal, "+
			"'bt up' starts it again and runs the seed hook again.")
	})

	t.Run("names a target given as a path by its UUID, which reads the same from any directory", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1, options: commands.RemoveOptions{Identifier: "."}}.remove(t)
		uuid := out.targetID
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far: nothing confirmed yet. "+
			"It is still registered, now pending: 'treerig worktree remove "+uuid+"' finishes the removal, "+
			"'treerig up' starts it again and runs the seed hook again.")
	})

	t.Run("keeps a target given as a host or a UUID as it was named", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1, options: commands.RemoveOptions{Identifier: host}}.remove(t)
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing "+host+". Removed so far: nothing confirmed yet. "+
			"It is still registered, now pending: 'treerig worktree remove "+host+"' finishes the removal, "+
			"'treerig up' starts it again and runs the seed hook again.")
	})

	t.Run("leaves out up when the target is another worktree", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1, others: true, options: commands.RemoveOptions{Identifier: "feature-other"}}.remove(t)
		wantNotice(t, out.lines, "Interrupted (SIGINT) while removing feature-other.acme.localhost. Removed so far: nothing confirmed yet. "+
			"It is still registered, now pending: 'treerig worktree remove "+out.targetID+"' finishes the removal.")
	})

	t.Run("keeps the registration and the home", func(t *testing.T) {
		out := interruption{match: " volume rm ", nth: 1}.remove(t)
		if !exists(t, out.paths.Home) || !exists(t, out.paths.RecordPath) {
			t.Fatal("the home or the registration went")
		}
	})

	t.Run("prints no notice when the lock could not be released after the signal", func(t *testing.T) {
		// The lock file is gone when it is released: that is reported on its
		// own, with the lock as the way to recover, and exits 1.
		out := interruption{match: " volume rm ", nth: 1, onInterrupt: func(paths layout.Worktree) {
			if err := os.Remove(paths.LockPath); err != nil {
				t.Errorf("removing the lock file: %v", err)
			}
		}}.remove(t)
		if out.err == nil {
			t.Fatal("RemoveWithContext = nil, want an error")
		}
		var cancelled *clierr.Cancelled
		if errors.As(out.err, &cancelled) {
			t.Fatalf("err = %v, want one that carries the lock failure too, not the interruption alone", out.err)
		}
		if got := interruptedLines(out.lines); len(got) != 0 {
			t.Fatalf("notice = %q, want none", got)
		}
	})

	t.Run("prints no notice when the signal lands before the worktree is pending", func(t *testing.T) {
		// `compose down` is the last step before the registration is marked.
		out := interruption{match: " compose ", nth: 1, services: []string{"api"}}.remove(t)
		if out.err == nil {
			t.Fatal("RemoveWithContext = nil, want the cancellation")
		}
		if out.state != registry.Complete {
			t.Fatalf("initialization = %v, want complete: nothing was destroyed", out.state)
		}
		if got := interruptedLines(out.lines); len(got) != 0 {
			t.Fatalf("notice = %q, want none", got)
		}
	})

	t.Run("prints no notice for a failure that no signal caused", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		volume := w.Docker.Owned(ref, brand.LabelNamespace)[1]
		foreign := commandstest.Ownership(ref, brand.LabelNamespace)
		foreign[brand.WorktreeLabel(brand.LabelNamespace)] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
		w.Docker.TamperAfter(volume, 3, foreign)
		tc := w.Context("", acme, nil)

		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err == nil {
			t.Fatal("RemoveWithContext = nil, want the refusal")
		}
		lines, _ := tc.Logger.Snapshot()
		if got := interruptedLines(lines); len(got) != 0 {
			t.Fatalf("notice = %q, want none", got)
		}
	})
}
