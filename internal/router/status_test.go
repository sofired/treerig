package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/router"
)

func TestRouterServiceStatusAndDown(t *testing.T) {
	t.Run("reports a stack it cannot inspect as unverifiable rather than clean", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.deleteDoc("api-1") // the containers are listed; one of them says nothing

		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || problems[0] != "the router could not be inspected" {
			t.Errorf("problems = %v", problems)
		}
	})

	t.Run("reports a router that is not running", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{Reachable: true, Running: false, Problems: []string{"the router is not running"}, Port: 80, Network: testNetwork}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("names the containers a half-present stack is missing rather than calling it stopped", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{"router": "router-1"}
		w.Fake.mu.Unlock()

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{
			Reachable: true, Running: true,
			Problems: []string{"docker-api: container is missing", "index: container is missing"},
			Port:     80, Network: testNetwork,
		}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("says nothing is missing when every container is there", func(t *testing.T) {
		w := newWorld(t, 80, "")
		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			if strings.Contains(p, "container is missing") {
				t.Errorf("did not expect a missing-container problem, got %v", problems)
			}
		}
	})

	t.Run("reports Docker as unreachable when `compose ps` fails for a reason it does not recognize", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.psExitCode = 125
		w.Fake.psStderr = "context \"desktop-linux\" not found\n"

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.Reachable || status.Running {
			t.Errorf("status = %+v", status)
		}
		if len(status.Problems) != 1 || status.Problems[0] != `Docker is not available: context "desktop-linux" not found` {
			t.Errorf("problems = %v", status.Problems)
		}
	})

	t.Run("reports Docker as unreachable, not the router as stopped, when Compose cannot talk to the daemon", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.psExitCode = 1
		w.Fake.psStderr = "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct " +
			"and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory\n"

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{
			Reachable: false, Running: false,
			Problems: []string{"Docker is not available: failed to connect to the docker API at unix:///var/run/docker.sock; " +
				"check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory"},
			Port: 80, Network: testNetwork,
		}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("asks Docker about the router by project name alone, so a missing stack file is never mistaken for a missing daemon", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if _, err := w.Router.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		var ps []string
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "ps") {
				ps = c.args
				break
			}
		}
		want := []string{"compose", "--project-name", testProject, "ps", "--all", "--quiet", "router"}
		if strings.Join(ps, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("ps args = %v, want %v", ps, want)
		}
	})

	t.Run("reports the line Docker meant, not the warning it printed first", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.psExitCode = 1
		w.Fake.psStderr = "WARNING: Error parsing config file (/home/me/.docker/config.json): invalid character\n" +
			"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.Reachable {
			t.Fatalf("status = %+v", status)
		}
		want := "Docker is not available: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
		if len(status.Problems) != 1 || status.Problems[0] != want {
			t.Errorf("problems = %v, want [%q]", status.Problems, want)
		}
	})

	t.Run("reports the reason, not the help hint Docker prints after it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.psExitCode = 1
		w.Fake.psStderr = "docker: unknown command: docker compose\n\nRun 'docker --help' for more information\n"

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := "Docker is not available: docker: unknown command: docker compose"
		if status.Reachable || len(status.Problems) != 1 || status.Problems[0] != want {
			t.Errorf("status = %+v, want problems [%q]", status, want)
		}
	})

	t.Run("does not blame the port when the socket is not ours", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"

		err := w.Router.Ensure(context.Background())
		want := "Docker is not available: permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
	})

	t.Run("says that docker is not installed instead of blaming the port when it cannot be started", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.dockerMissing = true

		err := w.Router.Ensure(context.Background())
		want := "Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?"
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.Reachable || status.Running {
			t.Errorf("status = %+v", status)
		}
	})

	t.Run("takes the router down by project name alone, so a control home with no stack file can still free it", func(t *testing.T) {
		w := newWorld(t, 80, "")

		if err := w.Router.Down(context.Background()); err != nil {
			t.Fatal(err)
		}

		var down []string
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "down") {
				down = c.args
				break
			}
		}
		want := []string{"compose", "--project-name", testProject, "down", "--remove-orphans"}
		if strings.Join(down, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("down args = %v, want %v", down, want)
		}
		exists, err := files.PathExists(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("expected nothing to have been written first")
		}
	})

	t.Run("treats the shared network staying behind as the expected outcome of down", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.downExitCode = 1
		w.Fake.downStderr = "Error response from daemon: error while removing network: network berth-router has active endpoints"

		if err := w.Router.Down(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "info: Network "+testNetwork+" is kept while worktree services are attached to it.")

		w.Fake.downStderr = "permission denied"
		err := w.Router.Down(context.Background())
		if err == nil || !strings.Contains(err.Error(), "did not stop cleanly") {
			t.Fatalf("got %v", err)
		}
	})
}

func statusEqual(got, want router.RouterStatus) bool {
	if got.Reachable != want.Reachable || got.Running != want.Running || got.Port != want.Port || got.Network != want.Network {
		return false
	}
	if len(got.Problems) != len(want.Problems) {
		return false
	}
	for i := range got.Problems {
		if got.Problems[i] != want.Problems[i] {
			return false
		}
	}
	return true
}

func TestRouterStatusReport(t *testing.T) {
	base := router.RouterStatus{Port: 80, Network: "berth-router"}

	t.Run("says nothing on stdout when Docker could not be asked, and fails", func(t *testing.T) {
		status := base
		status.Reachable = false
		status.Running = false
		status.Problems = []string{"Docker is not available: the `docker` command is not on PATH (ENOENT)."}

		report := router.StatusReport(status)
		want := router.Report{
			Output: []string{},
			Errors: []string{"Router state unknown: Docker is not available: the `docker` command is not on PATH (ENOENT)"},
			Failed: true,
		}
		if !reportEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}
	})

	t.Run("will not call a drifted stack running on the stream a script reads", func(t *testing.T) {
		// `running` only means a container exists; these two say it is not serving.
		status := base
		status.Reachable = true
		status.Running = true
		status.Problems = []string{"router: not running", "router: not healthy"}

		report := router.StatusReport(status)
		want := router.Report{
			Output: []string{"degraded on 127.0.0.1:80, network berth-router (2 problem(s) reported on stderr)"},
			Errors: []string{"Router problem: router: not running", "Router problem: router: not healthy"},
			Failed: true,
		}
		if !reportEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}
	})

	t.Run("reports a clean router, and a stopped one, without failing", func(t *testing.T) {
		clean := base
		clean.Reachable = true
		clean.Running = true
		clean.Problems = nil
		report := router.StatusReport(clean)
		want := router.Report{Output: []string{"running on 127.0.0.1:80, network berth-router"}, Errors: []string{}, Failed: false}
		if !reportEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}

		stopped := base
		stopped.Reachable = true
		stopped.Running = false
		stopped.Problems = []string{"the router is not running"}
		report2 := router.StatusReport(stopped)
		want2 := router.Report{Output: []string{"not running (would use 127.0.0.1:80, network berth-router)"}, Errors: []string{}, Failed: false}
		if !reportEqual(report2, want2) {
			t.Errorf("report = %+v, want %+v", report2, want2)
		}
	})
}

func reportEqual(got, want router.Report) bool {
	if got.Failed != want.Failed {
		return false
	}
	if len(got.Output) != len(want.Output) || len(got.Errors) != len(want.Errors) {
		return false
	}
	for i := range got.Output {
		if got.Output[i] != want.Output[i] {
			return false
		}
	}
	for i := range got.Errors {
		if got.Errors[i] != want.Errors[i] {
			return false
		}
	}
	return true
}
