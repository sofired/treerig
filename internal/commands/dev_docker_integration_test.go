//go:build integration

package commands_test

// `dev` against a real daemon: the one behaviour a fake Docker cannot show,
// what Compose's own log follower does when the project's containers go away.
// On Compose 5.5.0 it sometimes keeps waiting for new ones (seen when a
// `restart: always` service had restarted under it, never on 2.38.2),
// so the session outlives a `down` and keeps refusing `worktree remove` until
// it is ended, and sometimes exits 0 and the session ends. This test pins the
// idle case when Compose goes that way, and in either case that the lock agrees
// with the follower.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
)

// devCompose is walkCompose plus a `restart: always` service that exits every
// few seconds, which is what makes Compose 5.5.0's follower keep waiting after
// a `down` when it restarted under the follower; it gives the idle path a
// chance, and the test accepts either outcome.
const devCompose = `services:
  api:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from api' > /www/index.html && exec httpd -f -p 8080 -h /www"]
    expose: ["8080"]
  flaky:
    image: busybox:1.37.0
    command: ["sh", "-c", "sleep 3; exit 0"]
    restart: always
`

// followerRunning reports whether a `compose logs --follow` of project is a
// live process.
func followerRunning(project string) bool {
	return exec.Command("pgrep", "-f", "project-name "+project+" logs --follow").Run() == nil
}

func TestDevOnARealDaemon(t *testing.T) {
	t.Run("a session whose project has a restarting service stays consistent with its follower after down", func(t *testing.T) {
		devSessionAfterDown(t, devCompose)
	})
	t.Run("a session whose containers were all running stays consistent with its follower after down", func(t *testing.T) {
		devSessionAfterDown(t, walkCompose)
	})
}

// devSessionAfterDown runs `dev` against a real daemon, runs `down` during it,
// and checks what became of the session. Whether Compose's follower ends
// with its containers varies (see backend.Compose.Logs), so either outcome
// is accepted, as long as it is a consistent one. A session still attached
// keeps its follower and its development lock, refuses a removal with the
// advice to press Ctrl-C, and is ended by a signal; one that ended says the
// follower ended on its own and leaves nothing held or running.
func devSessionAfterDown(t *testing.T, compose string) {
	w := newWalkFixture(t, "dev", "feature/walk", walkToml, compose)
	if err := os.MkdirAll(filepath.Join(w.checkout, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(w.checkout, "hooks", "record"), fmt.Sprintf("#!/bin/sh\necho \"$TREERIG_STAGE\" >> %q\n", w.log), 0o755)
	const host = "feature-walk.walk.localhost"
	cctx := w.context(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() {
		done <- commands.DevWithContext(ctx, cctx, commands.SharedRouter(w.env, w.svc, w.state), commands.BestEffortIndex(cctx))
	}()

	// Wait for the session to be following.
	var registrationErr error
	var paths layout.Worktree
	var project string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		select {
		case err := <-done:
			t.Fatalf("dev ended before it followed anything: %v", err)
		default:
		}
		registration, err := w.context(t).Registry.Current(context.Background())
		registrationErr = err
		if err == nil && registration != nil {
			paths = layout.WorktreeLayout(w.controlHome, registration.RepositoryID, registration.WorktreeID)
			project = layout.ComposeProjectName(registration.RepositoryID, registration.WorktreeID)
			if followerRunning(project) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dev never started following (registration error: %v)", registrationErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !flockHeld(t, paths.DevelopmentLockPath) || flockHeld(t, paths.LockPath) {
		t.Fatal("while following, the development lock must be held and the operation lock not")
	}
	w.eventually(t, host, func(status int, body string) bool { return status == 200 && body == "hello from api\n" })

	// `down` from "another terminal" works during the session and removes every container.
	if err := commands.DownWithContext(context.Background(), w.context(t)); err != nil {
		t.Fatalf("down during a session: %v", err)
	}

	// Give the follower, and the poll for containers, time to react.
	var ended bool
	var endErr error
	select {
	case endErr = <-done:
		ended = true
	case <-time.After(8 * time.Second): // longer than one poll of the containers
	}
	if ended {
		// Compose's follower went with its containers: the session says so,
		// and nothing is left held or running.
		if endErr == nil || !strings.Contains(endErr.Error(), "ended on its own (exit 0)") {
			t.Fatalf("the session ended with %v, want the follower's ending on its own", endErr)
		}
	} else {
		if !followerRunning(project) || !flockHeld(t, paths.DevelopmentLockPath) {
			t.Fatal("an idle session must keep its log follower and its development lock")
		}
		_, err := commands.RemoveWithContext(context.Background(), w.context(t), w.env, commands.RemoveOptions{Yes: true})
		if err == nil || !strings.Contains(err.Error(), "development session") ||
			!strings.Contains(err.Error(), "End it with Ctrl-C") {
			t.Fatalf("worktree remove during an idle session = %v, want the development-session refusal advising Ctrl-C", err)
		}
		// Ending it stops the follower and frees the lock.
		cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
		select {
		case endErr = <-done:
			var cancelled *clierr.Cancelled
			if !errors.As(endErr, &cancelled) {
				t.Fatalf("an ended session returned %v, want the cancellation", endErr)
			}
		case <-time.After(time.Minute):
			t.Fatal("the session never ended after it was cancelled")
		}
	}
	if followerRunning(project) || flockHeld(t, paths.DevelopmentLockPath) {
		t.Fatal("after the session ended, the follower or the development lock is left")
	}

	// And nothing is left over: a removal now works.
	result := w.up(t)
	w.downAndRemove(t, result, host)
}
