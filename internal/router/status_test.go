package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/router"
)

func TestRouterServiceStatusAndDown(t *testing.T) {
	t.Run("reports a stack it cannot inspect as unverifiable rather than clean", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.deleteDoc("index-1") // the containers are listed; one of them says nothing

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := status.Messages(); len(got) != 1 || got[0] != "the router could not be inspected" {
			t.Errorf("problems = %v", got)
		}
	})

	t.Run("reports a router that is not running", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.setIDs(map[string]string{})

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{Reachable: true, Running: false, Problems: []router.Problem{{Code: "router.absent", Message: "the router is not running"}}, Port: 80}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("names the containers a half-present stack is missing rather than calling it stopped", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.setIDs(map[string]string{"router": "router-1"})

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{
			Reachable: true, Running: true,
			Problems: []router.Problem{{Code: "index.missing", Message: "index: container is missing"}},
			Port:     80,
		}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("says nothing is missing when every container is there", func(t *testing.T) {
		w := newWorld(t, 80, "")
		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range status.Messages() {
			if strings.Contains(p, "container is missing") {
				t.Errorf("did not expect a missing-container problem, got %v", status.Messages())
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
		if len(status.Problems) != 1 || status.Problems[0] != (router.Problem{Code: "stack.unreachable", Message: `Docker is not available: context "desktop-linux" not found`}) {
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
			Problems: []router.Problem{{Code: "stack.unreachable", Message: "Docker is not available: failed to connect to the docker API at unix:///var/run/docker.sock; " +
				"check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory"}},
			Port: 80,
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
		if len(status.Problems) != 1 || status.Problems[0].Message != want {
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
		if status.Reachable || len(status.Problems) != 1 || status.Problems[0].Message != want {
			t.Errorf("status = %+v, want problems [%q]", status, want)
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

	t.Run("reports a refusal of Compose, with its own words", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.downExitCode = 1
		w.Fake.downStderr = "permission denied"
		err := w.Router.Down(context.Background())
		if err == nil || !strings.Contains(err.Error(), "did not stop cleanly") || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("got %v", err)
		}
	})

	// A timeout is reported through run.Failure, with its own exit code 124,
	// not the plain-refusal wording.
	t.Run("keeps run.Failure's own timeout code, not stderr's wording, for a compose down that times out", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.downTimedOut = true
		w.Fake.downStderr = "Error response from daemon: error while removing network"

		err := w.Router.Down(context.Background())
		if err == nil {
			t.Fatal("expected a compose down that times out to fail")
		}
		if strings.Contains(err.Error(), "did not stop cleanly") {
			t.Fatalf("expected run.Failure's own wording, not the plain-refusal string, got %v", err)
		}
		if clierr.Code(err) != 124 {
			t.Fatalf("code = %d, want 124 (a timeout), err: %v", clierr.Code(err), err)
		}
	})
}

// statusEqual reports whether got and want are the same RouterStatus.
func statusEqual(got, want router.RouterStatus) bool {
	if got.Reachable != want.Reachable || got.Running != want.Running || got.Port != want.Port || got.RunningPort != want.RunningPort || got.Mode != want.Mode {
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
	base := router.RouterStatus{Port: 80}

	t.Run("says nothing on stdout when Docker could not be asked, and fails", func(t *testing.T) {
		status := base
		status.Reachable = false
		status.Running = false
		status.Problems = []router.Problem{{Code: "stack.unreachable", Message: "Docker is not available: the `docker` command is not on PATH (ENOENT)."}}

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
		status.Problems = []router.Problem{{Code: "router.not-running", Message: "router: not running"}, {Code: "router.not-healthy", Message: "router: not healthy"}}

		report := router.StatusReport(status)
		want := router.Report{
			Output: []string{"degraded on 127.0.0.1:80, mode unknown (2 problem(s) reported on stderr)"},
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
		want := router.Report{Output: []string{"running on 127.0.0.1:80, mode unknown"}, Errors: []string{}, Failed: false}
		if !reportEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}

		hosted := clean
		hosted.Mode = router.ModeHost
		hosted.RunningPort = 8080
		report = router.StatusReport(hosted)
		want = router.Report{Output: []string{"running on 127.0.0.1:8080, host mode"}, Errors: []string{}, Failed: false}
		if !reportEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}

		stopped := base
		stopped.Reachable = true
		stopped.Running = false
		stopped.Problems = []router.Problem{{Code: "router.absent", Message: "the router is not running"}}
		report2 := router.StatusReport(stopped)
		want2 := router.Report{Output: []string{"not running (would use 127.0.0.1:80)"}, Errors: []string{}, Failed: false}
		if !reportEqual(report2, want2) {
			t.Errorf("report = %+v, want %+v", report2, want2)
		}
	})
}

// reportEqual reports whether got and want are the same Report.
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

func TestRouterStatusReadsTheContainers(t *testing.T) {
	t.Run("reports the mode and port the running router was started with", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := router.RouterStatus{Reachable: true, Running: true, Port: 8080, RunningPort: 8080, Mode: router.ModeHost}
		if !statusEqual(status, want) {
			t.Errorf("status = %+v, want %+v", status, want)
		}
	})

	t.Run("reports a container that is not running or not healthy by service, once each", func(t *testing.T) {
		w := newWorld(t, 80, "")
		bad := indexDoc()
		bad.State = router.InspectState{Status: "restarting", Health: router.InspectHealth{Status: "unhealthy"}}
		w.Fake.setDoc("index-1", bad)
		sick := routerDoc(router.ModeHost, 80)
		sick.State.Health.Status = "starting"
		w.Fake.setDoc("router-1", sick)

		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var codes []string
		for _, problem := range status.Problems {
			codes = append(codes, problem.Code)
		}
		want := []string{"router.not-healthy", "index.not-running", "index.not-healthy"}
		if strings.Join(codes, ",") != strings.Join(want, ",") {
			t.Errorf("codes = %v, want %v", codes, want)
		}
		if !status.Running {
			t.Error("a degraded stack is still running")
		}
	})

	t.Run("does not read a configuration that differs from this build's as a problem: Compose recreates it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		doc := routerDoc(router.ModeHost, 80)
		doc.Config.Cmd = []string{"--something=else"}
		doc.HostConfig.NetworkMode = "bridge"
		w.Fake.setDoc("router-1", doc)
		status, err := w.Router.Status(context.Background())
		if err != nil || len(status.Problems) != 0 {
			t.Fatalf("status = %+v, %v", status, err)
		}
	})
}
