package commands

import (
	"errors"
	"syscall"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/worktree"
)

func TestInterruptNotice(t *testing.T) {
	sigint := &clierr.Cancelled{Signal: syscall.SIGINT}
	sighup := &clierr.Cancelled{Signal: syscall.SIGHUP}
	volume := []worktree.OwnedResource{{Kind: "volume", ID: "v1"}}
	both := nextSteps{finish: "berth worktree remove", restart: "berth up"}
	const host = "feature-login.acme.localhost"
	const prefix = "Interrupted (SIGINT) while removing " + host + ". "

	for _, tc := range []struct {
		name   string
		marked bool
		cause  error
		err    error
		result *RemoveResult
		next   nextSteps
		want   string
	}{
		{
			name: "resources and the home directory, with both next steps", marked: true, cause: sigint, err: sigint,
			result: &RemoveResult{Resources: volume, Home: true}, next: both,
			want: prefix + "Removed so far (at least): 1 volume, home directory. It is still registered, now pending: " +
				"'berth worktree remove' finishes the removal, 'berth up' starts it again and runs the seed hook again.",
		},
		{
			name: "the home directory alone", marked: true, cause: sigint, err: sigint,
			result: &RemoveResult{Home: true}, next: both,
			want: prefix + "Removed so far (at least): home directory. It is still registered, now pending: " +
				"'berth worktree remove' finishes the removal, 'berth up' starts it again and runs the seed hook again.",
		},
		{
			name: "nothing confirmed, and no up for another worktree", marked: true, cause: sigint, err: sigint,
			result: &RemoveResult{}, next: nextSteps{finish: "berth worktree remove other"},
			want: prefix + "Removed so far: nothing confirmed yet. It is still registered, now pending: " +
				"'berth worktree remove other' finishes the removal.",
		},
		{
			name: "the signal named is the cause's, not the error's", marked: true, cause: sighup, err: sigint,
			result: &RemoveResult{}, next: both,
			want: "Interrupted (SIGHUP) while removing " + host + ". Removed so far: nothing confirmed yet. It is still registered, now pending: " +
				"'berth worktree remove' finishes the removal, 'berth up' starts it again and runs the seed hook again.",
		},
		{name: "the registration was never marked pending", marked: false, cause: sigint, err: sigint, result: &RemoveResult{}, next: both},
		{name: "the registration is already gone", marked: true, cause: sigint, err: sigint, result: &RemoveResult{Registration: true}, next: both},
		{name: "no signal ended the context", marked: true, cause: nil, err: sigint, result: &RemoveResult{}, next: both},
		{name: "the context ended for another reason", marked: true, cause: errors.New("deadline"), err: sigint, result: &RemoveResult{}, next: both},
		{name: "the error is something else, whatever the cause", marked: true, cause: sigint, err: errors.New("refused"), result: &RemoveResult{}, next: both},
		{name: "the error was rebuilt around a release failure, as lifecycle.attach does", marked: true, cause: sigint, err: &clierr.Error{Message: sigint.Error(), ExitCode: 1, Cause: errors.New("lock released late")}, result: &RemoveResult{}, next: both},
		{name: "there is no error", marked: true, cause: sigint, err: nil, result: &RemoveResult{Registration: true}, next: both},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := interruptNotice(tc.marked, tc.cause, tc.err, tc.result, host, tc.next); got != tc.want {
				t.Fatalf("notice = %q\nwant     %q", got, tc.want)
			}
		})
	}

	t.Run("filters the host", func(t *testing.T) {
		got := interruptNotice(true, sigint, sigint, &RemoveResult{}, "evil\x1b[2Jhost", both)
		if want := "Interrupted (SIGINT) while removing evil\ufffd[2Jhost."; got[:len(want)] != want {
			t.Fatalf("notice = %q, want it to start %q", got, want)
		}
	})
}

func TestRemoveNextSteps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options RemoveOptions
		here    bool
		want    nextSteps
	}{
		{"this checkout", RemoveOptions{}, true, nextSteps{finish: "berth worktree remove", restart: "berth up"}},
		{"by slug, elsewhere", RemoveOptions{Identifier: "feature-login"}, false, nextSteps{finish: "berth worktree remove feature-login"}},
		{"by slug in a domain", RemoveOptions{Identifier: "feature-login", Domain: "acme.localhost"}, false,
			nextSteps{finish: "berth worktree remove feature-login --domain acme.localhost"}},
		{"with a label namespace", RemoveOptions{Identifier: "feature-login", LabelNamespace: "dev.acme"}, false,
			nextSteps{finish: "berth worktree remove feature-login --label-namespace dev.acme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := removeNextSteps("berth", tc.options, tc.here); got != tc.want {
				t.Fatalf("removeNextSteps = %+v, want %+v", got, tc.want)
			}
		})
	}
}
