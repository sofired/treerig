package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/fakerun"
)

// answering is a runner whose every call succeeds (in the run.Runner sense)
// with result, whatever the command.
func answering(result run.Result) *fakerun.Runner {
	return &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) { return result, nil }}
}

// refusing is a runner whose every call fails outright with err, as a runner
// does when the command never started.
func refusing(err error) *fakerun.Runner {
	return &fakerun.Runner{Handler: func(context.Context, run.Spec) (run.Result, error) { return run.Result{}, err }}
}

// wantError fails the test unless err is non-nil and its message contains message.
func wantError(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("error = %v, want one containing %q", err, message)
	}
}

func TestSaid(t *testing.T) {
	t.Run("skips the warnings Docker prints before the reason", func(t *testing.T) {
		stderr := "WARNING: Error parsing config file (/home/me/.docker/config.json): invalid character\n" +
			"time=\"2026-09-21T10:00:00Z\" level=warning msg=\"something\"\n" +
			"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"
		if got := Said(stderr, "fallback"); got != "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" {
			t.Fatal(got)
		}
	})
	t.Run("skips the help hint Docker prints after the reason", func(t *testing.T) {
		if got := Said("docker: 'compose' is not a docker command.\nSee 'docker --help'\n", "fallback"); got != "docker: 'compose' is not a docker command." {
			t.Fatal(got)
		}
		if got := Said("docker: unknown command: docker compose\n\nRun 'docker --help' for more information\n", "fallback"); got != "docker: unknown command: docker compose" {
			t.Fatal(got)
		}
	})
	t.Run("settles for the last warning when that is all there is, and the fallback when there is nothing", func(t *testing.T) {
		if got := Said("WARN[0000] first\nWARN[0001] second\n", "fallback"); got != "WARN[0001] second" {
			t.Fatal(got)
		}
		if got := Said("  \n", "fallback"); got != "fallback" {
			t.Fatal(got)
		}
	})
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	t.Run("reports a daemon that cannot be reached as Docker being unavailable, whatever the caller asked for", func(t *testing.T) {
		runner := answering(run.Result{ExitCode: 1, Stderr: "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running\n"})
		_, err := Run(ctx, runner, []string{"ps"}, run.Spec{AllowFailure: true})
		if !Unavailable(err) {
			t.Fatalf("error = %v, want UnavailableError", err)
		}
		wantError(t, err, "Docker is not available: failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running")
	})
	t.Run("reports a missing Compose plugin the same way", func(t *testing.T) {
		runner := answering(run.Result{ExitCode: 1, Stderr: "docker: 'compose' is not a docker command.\nSee 'docker --help'\n"})
		_, err := Run(ctx, runner, []string{"compose", "ps"}, run.Spec{AllowFailure: true})
		wantError(t, err, "Docker is not available: docker: 'compose' is not a docker command.")
	})
	t.Run("reports a docker that is not installed the same way, with the obvious question", func(t *testing.T) {
		runner := refusing(&run.StartError{Command: "docker", Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127})
		_, err := Run(ctx, runner, []string{"ps"}, run.Spec{})
		wantError(t, err, "Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?")
	})
	t.Run("reports the complaint line itself, not the progress Compose printed before the daemon went away", func(t *testing.T) {
		runner := answering(run.Result{ExitCode: 1, Stderr: " Network acme_default  Creating\n Container acme-db  Created\nerror during connect: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json\": EOF\n"})
		_, err := Run(ctx, runner, []string{"compose", "up"}, run.Spec{AllowFailure: true})
		wantError(t, err, "Docker is not available: error during connect: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json\": EOF")
	})
	t.Run("does not mistake a build step that talks about Docker for Docker itself", func(t *testing.T) {
		stderr := "#5 [2/3] RUN docker version\n#5 0.412 Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n#5 ERROR: process \"/bin/sh -c docker version\" did not complete successfully: exit code: 1\n"
		if _, ok := Complaint(stderr); ok {
			t.Fatal("a build step's output was taken for Docker's own complaint")
		}
		result, err := Run(ctx, answering(run.Result{ExitCode: 17, Stderr: stderr}), []string{"compose", "up", "--build"}, run.Spec{AllowFailure: true})
		if err != nil || result.ExitCode != 17 {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
		_, err = Run(ctx, answering(run.Result{ExitCode: 17, Stderr: stderr}), []string{"compose", "up", "--build"}, run.Spec{})
		wantError(t, err, "Command failed (docker compose up --build): #5 [2/3] RUN docker version")
	})
	t.Run("recognises a socket that is not ours, in the wording of Docker before and after 28", func(t *testing.T) {
		before := answering(run.Result{ExitCode: 1, Stderr: "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.24/containers/json\": dial unix /var/run/docker.sock: connect: permission denied\n"})
		if _, err := Run(ctx, before, []string{"ps"}, run.Spec{AllowFailure: true}); !Unavailable(err) {
			t.Fatalf("before 28: %v", err)
		}
		after := answering(run.Result{ExitCode: 1, Stderr: "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"})
		_, err := Run(ctx, after, []string{"compose", "up"}, run.Spec{AllowFailure: true})
		wantError(t, err, "Docker is not available: permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")
		engine20 := answering(run.Result{ExitCode: 1, Stderr: "Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: connect: permission denied\n"})
		if _, err := Run(ctx, engine20, []string{"ps"}, run.Spec{AllowFailure: true}); !Unavailable(err) {
			t.Fatalf("engine 20: %v", err)
		}
	})
	t.Run("keeps the words for a docker that exists but cannot run", func(t *testing.T) {
		runner := refusing(&run.StartError{Command: "docker", Code: "EACCES", Detail: "is not executable (EACCES).", ExitCode: 126})
		_, err := Run(ctx, runner, []string{"ps"}, run.Spec{})
		wantError(t, err, "Docker is not available: the `docker` command is not executable (EACCES).")
	})
	t.Run("recognises Docker Desktop while it is still starting, in each of its wordings", func(t *testing.T) {
		for _, line := range []string{
			"Error response from daemon: dial unix docker.raw.sock: connect: connection refused",
			"Error response from daemon: Bad response from Docker engine",
			"request returned Internal Server Error for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json, check if the server supports the requested API version",
		} {
			_, err := Run(ctx, answering(run.Result{ExitCode: 1, Stderr: line + "\n"}), []string{"ps"}, run.Spec{AllowFailure: true})
			wantError(t, err, "Docker is not available: "+line)
		}
	})
	t.Run("honours a timeout the way the runner does", func(t *testing.T) {
		runner := answering(run.Result{TimedOut: true})
		_, err := Run(ctx, runner, []string{"ps"}, run.Spec{Timeout: 5 * time.Millisecond})
		if clierr.Code(err) != 124 {
			t.Fatalf("code = %d (%v), want 124", clierr.Code(err), err)
		}
		result, err := Run(ctx, runner, []string{"ps"}, run.Spec{Timeout: 5 * time.Millisecond, AllowFailure: true})
		if err != nil || !result.TimedOut {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
	})
	t.Run("leaves an ordinary failed request to the caller — that is the request's verdict, not Docker's", func(t *testing.T) {
		runner := answering(run.Result{ExitCode: 1, Stderr: "Error response from daemon: No such container: abc\n"})
		result, err := Run(ctx, runner, []string{"inspect", "abc"}, run.Spec{AllowFailure: true})
		if err != nil || result.ExitCode != 1 {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
		_, err = Run(ctx, runner, []string{"inspect", "abc"}, run.Spec{})
		wantError(t, err, "Command failed (docker inspect abc): Error response from daemon: No such container: abc")
	})
	t.Run("reads Docker before honouring a caller that wanted failures thrown, passing everything else through", func(t *testing.T) {
		runner := answering(run.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"})
		_, err := Run(ctx, runner, []string{"compose", "down"}, run.Spec{Env: []string{"A=1"}, Output: run.Tee, Timeout: 5 * time.Millisecond})
		wantError(t, err, "Docker is not available: Cannot connect to the Docker daemon")
		calls := runner.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d", len(calls))
		}
		call := calls[0]
		if call.Command != "docker" || strings.Join(call.Args, " ") != "compose down" || !call.AllowFailure ||
			call.Output != run.Tee || call.Timeout != 5*time.Millisecond || strings.Join(call.Env, ",") != "A=1" || !call.InheritLocks {
			t.Fatalf("passed through %+v", call)
		}
	})
}

func TestComposeEnvironment(t *testing.T) {
	t.Run("drops the variables that would redirect Compose, and lets the caller's values win", func(t *testing.T) {
		got := ComposeEnvironment([]string{"PATH=/bin", "COMPOSE_FILE=x", "COMPOSE_PROGRESS=tty", "COMPOSE_IGNORE_ORPHANS=1"}, "COMPOSE_PROJECT_NAME=p")
		want := "PATH=/bin,COMPOSE_PROGRESS=plain,COMPOSE_PROJECT_NAME=p"
		if strings.Join(got, ",") != want {
			t.Fatalf("got %v, want %s", got, want)
		}
	})
}
