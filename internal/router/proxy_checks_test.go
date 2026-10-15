package router_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/router"
)

// desktopInfo is what `docker info --format '{{json .}}'` prints on Docker Desktop.
const desktopInfo = `{"Name":"docker-desktop","OperatingSystem":"Docker Desktop","OSType":"linux","SecurityOptions":["name=seccomp,profile=unconfined"]}`

// rootlessInfo is what a rootless Linux engine prints.
const rootlessInfo = `{"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux","SecurityOptions":["name=rootless"]}`

func TestModeChoice(t *testing.T) {
	ctx := context.Background()

	t.Run("detects the mode when nothing overrides it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		choice, err := w.Router.ModeChoice(ctx)
		if err != nil || choice.Mode != router.ModeHost || choice.Detected != router.ModeHost || choice.Override != "" || choice.DetectErr != nil {
			t.Fatalf("ModeChoice = %+v, %v", choice, err)
		}
	})

	t.Run("names an override that agrees with detection", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "host" })
		choice, err := w.Router.ModeChoice(ctx)
		if err != nil || choice.Mode != router.ModeHost || choice.Detected != router.ModeHost || choice.Override != router.ModeHost {
			t.Fatalf("ModeChoice = %+v, %v", choice, err)
		}
	})

	t.Run("keeps what detection would pick beside an override that disagrees", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "bridge" })
		choice, err := w.Router.ModeChoice(ctx)
		if err != nil || choice.Mode != router.ModeBridge || choice.Detected != router.ModeHost || choice.Override != router.ModeBridge {
			t.Fatalf("ModeChoice = %+v, %v", choice, err)
		}
	})

	t.Run("uses the override where detection alone would refuse, and says why it would", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "bridge"; o.ClientOS = "darwin" })
		w.Fake.info = strings.Replace(linuxInfo, "cc-worker", "colima", 1)
		choice, err := w.Router.ModeChoice(ctx)
		if err != nil || choice.Mode != router.ModeBridge || choice.Detected != "" || choice.DetectErr == nil {
			t.Fatalf("ModeChoice = %+v, %v", choice, err)
		}
	})

	t.Run("refuses a remote daemon even with an override", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "bridge"; o.DockerHost = "tcp://10.0.0.5:2375" })
		if _, err := w.Router.ModeChoice(ctx); err == nil || !strings.Contains(err.Error(), "tcp://10.0.0.5:2375") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("refuses a rootless daemon even with an override", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "host" })
		w.Fake.info = rootlessInfo
		if _, err := w.Router.ModeChoice(ctx); err == nil || !strings.Contains(err.Error(), "#121") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("reports a detection refusal when nothing overrides it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.info = rootlessInfo
		if _, err := w.Router.ModeChoice(ctx); err == nil {
			t.Fatal("expected the refusal")
		}
	})

	t.Run("picks bridge for Docker Desktop", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.info = desktopInfo
		choice, err := w.Router.ModeChoice(ctx)
		if err != nil || choice.Mode != router.ModeBridge {
			t.Fatalf("ModeChoice = %+v, %v", choice, err)
		}
	})
}

func TestStatusReportsTheLayoutOfTheRunningRouter(t *testing.T) {
	ctx := context.Background()
	problemWithCode := func(status router.RouterStatus, code string) (router.Problem, bool) {
		for _, problem := range status.Problems {
			if problem.Code == code {
				return problem, true
			}
		}
		return router.Problem{}, false
	}
	protocolOf := func(t *testing.T, w *testWorld, label *string) {
		t.Helper()
		doc := routerDoc(router.ModeHost, 80)
		if label == nil {
			delete(doc.Config.Labels, testNS+".router-protocol")
		} else {
			doc.Config.Labels[testNS+".router-protocol"] = *label
		}
		w.Fake.setDoc("router-1", doc)
	}

	t.Run("says a router with no protocol label has to be stopped first", func(t *testing.T) {
		w := newWorld(t, 80, "")
		protocolOf(t, w, nil)
		status, err := w.Router.Status(ctx)
		problem, ok := problemWithCode(status, "router.protocol")
		if err != nil || !ok || !strings.Contains(problem.Message, "Run 'duke router down' first") {
			t.Fatalf("status = %+v, %v", status, err)
		}
		if strings.Contains(problem.Message, "recreate") {
			t.Errorf("the advice must not promise a recreate: %s", problem.Message)
		}
	})

	t.Run("says a router of a lower protocol has to be stopped first", func(t *testing.T) {
		w := newWorld(t, 80, "")
		lower := strconv.Itoa(router.Protocol - 1)
		protocolOf(t, w, &lower)
		status, _ := w.Router.Status(ctx)
		if problem, ok := problemWithCode(status, "router.protocol"); !ok || !strings.Contains(problem.Message, "Run 'duke router down' first") {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("gives the downgrade advice for a router of a higher protocol", func(t *testing.T) {
		w := newWorld(t, 80, "")
		higher := strconv.Itoa(router.Protocol + 1)
		protocolOf(t, w, &higher)
		status, _ := w.Router.Status(ctx)
		problem, ok := problemWithCode(status, "router.protocol")
		if !ok || !strings.Contains(problem.Message, "rather than downgrading") {
			t.Fatalf("status = %+v", status)
		}
		// The same words Ensure refuses with.
		err := w.Router.Ensure(ctx)
		if err == nil || err.Error() != problem.Message {
			t.Errorf("Ensure = %v, status says %q", err, problem.Message)
		}
	})

	t.Run("reports nothing for a router whose protocol matches, or one that is stopped", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if status, _ := w.Router.Status(ctx); len(status.Problems) != 0 {
			t.Fatalf("problems = %+v", status.Problems)
		}
		doc := routerDoc(router.ModeHost, 80)
		delete(doc.Config.Labels, testNS+".router-protocol")
		doc.State = router.InspectState{Status: "exited"}
		w.Fake.setDoc("router-1", doc)
		status, _ := w.Router.Status(ctx)
		if _, ok := problemWithCode(status, "router.protocol"); ok {
			t.Errorf("a stopped router serves nobody: %+v", status.Problems)
		}
	})
}

func TestStatusProblemsCarryTheContainersLog(t *testing.T) {
	ctx := context.Background()

	t.Run("attaches the compose log to a container that is not running or not healthy", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.logs = "index-1  | httpd: bind: address already in use\nindex-1  | exiting"
		bad := indexDoc()
		bad.State = router.InspectState{Status: "restarting", Health: router.InspectHealth{Status: "unhealthy"}}
		w.Fake.setDoc("index-1", bad)
		status, err := w.Router.Status(ctx)
		if err != nil || len(status.Problems) != 2 {
			t.Fatalf("status = %+v, %v", status, err)
		}
		for _, problem := range status.Problems {
			if problem.Log != w.Fake.logs {
				t.Errorf("%s: Log = %q", problem.Code, problem.Log)
			}
		}
		asked := false
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "logs") && c.args[len(c.args)-1] == "index" {
				asked = true
			}
		}
		if !asked {
			t.Error("the log was not asked for the unhealthy service")
		}
	})

	t.Run("leaves the log off a healthy stack", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.logs = "noise"
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "logs") {
				t.Fatal("logs were read before anything was wrong")
			}
		}
		if status, _ := w.Router.Status(ctx); len(status.Problems) != 0 {
			t.Fatalf("problems = %+v", status.Problems)
		}
		for _, c := range w.Fake.snapshotCalls() {
			if containsArg(c.args, "logs") {
				t.Fatal("logs were read for a healthy stack")
			}
		}
	})

	t.Run("prints the log lines under the problem in the status report", func(t *testing.T) {
		report := router.StatusReport(router.RouterStatus{
			Reachable: true, Running: true, Port: 80, RunningPort: 80, Mode: router.ModeHost,
			Problems: []router.Problem{{Code: "router.not-healthy", Message: "router: not healthy", Log: "first\nsecond"}},
		})
		if len(report.Errors) != 1 {
			t.Fatalf("errors = %q", report.Errors)
		}
		want := "Router problem: router: not healthy\n  Last log lines:\n    first\n    second"
		if report.Errors[0] != want {
			t.Errorf("error = %q, want %q", report.Errors[0], want)
		}
	})
}

func TestCheckPorts(t *testing.T) {
	ctx := context.Background()
	port := func(p int) string { return strconv.Itoa(p) }

	t.Run("finds nothing wrong when the ports are free", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		conflicts, err := w.Router.CheckPorts(ctx)
		if err != nil || len(conflicts) != 0 {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
	})

	t.Run("names the process holding the router port and says to stop it or choose another port", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[80] = true
		conflicts, err := w.Router.CheckPorts(ctx)
		if err != nil || len(conflicts) != 1 || conflicts[0].Port != 80 || conflicts[0].Container || conflicts[0].Movable {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
		line := conflicts[0].Describe()
		for _, want := range []string{"127.0.0.1:80 (the router) is held by another process", "ss -ltnp", "stop it, or choose another port"} {
			if !strings.Contains(line, want) {
				t.Errorf("%q lacks %q", line, want)
			}
		}
	})

	t.Run("shows how to find a container holding the router port too, and still says to stop it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[80] = true
		w.Fake.publishers["80"] = []string{"someone-else"}
		conflicts, err := w.Router.CheckPorts(ctx)
		if err != nil || len(conflicts) != 1 || !conflicts[0].Container || conflicts[0].Movable {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
		line := conflicts[0].Describe()
		for _, want := range []string{"held by container someone-else", "docker ps --filter publish=80", "stop it, or choose another port"} {
			if !strings.Contains(line, want) {
				t.Errorf("%q lacks %q", line, want)
			}
		}
	})

	t.Run("tells a container holding an allocated port to be found and the state file removed", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[seedIndexPort] = true
		w.Fake.publishers[port(seedIndexPort)] = []string{"someone-else"}
		conflicts, err := w.Router.CheckPorts(ctx)
		if err != nil || len(conflicts) != 1 || !conflicts[0].Container || !conflicts[0].Movable {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
		line := conflicts[0].Describe()
		for _, want := range []string{"(the checkout index) is held by container someone-else", "remove " + w.State.StateFile,
			"docker ps --filter publish=" + port(seedIndexPort)} {
			if !strings.Contains(line, want) {
				t.Errorf("%q lacks %q", line, want)
			}
		}
		if strings.Contains(line, "stop it") {
			t.Errorf("a movable port should not be told to stop its holder: %q", line)
		}
	})

	t.Run("checks the ping port in host mode only", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		w.Probe.Busy[seedPingPort] = true
		if conflicts, _ := w.Router.CheckPorts(ctx); len(conflicts) != 1 || conflicts[0].What != "the router's ping" {
			t.Fatalf("host mode: %+v", conflicts)
		}
		bridge := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "bridge" })
		stopped(t, bridge, router.ModeBridge)
		bridge.Probe.Busy[seedPingPort] = true
		if conflicts, _ := bridge.Router.CheckPorts(ctx); len(conflicts) != 0 {
			t.Fatalf("bridge mode: %+v", conflicts)
		}
	})

	t.Run("skips the ports the stack's own running containers hold", func(t *testing.T) {
		w := newWorld(t, 80, "")
		for _, held := range []int{80, seedIndexPort, seedPingPort} {
			w.Probe.Busy[held] = true
		}
		conflicts, err := w.Router.CheckPorts(ctx)
		if err != nil || len(conflicts) != 0 {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
	})

	t.Run("checks only the router port before a state file exists", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		if err := os.Remove(w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		w.Probe.Busy[seedIndexPort] = true
		w.Probe.Busy[seedPingPort] = true
		if conflicts, err := w.Router.CheckPorts(ctx); err != nil || len(conflicts) != 0 {
			t.Fatalf("CheckPorts = %+v, %v", conflicts, err)
		}
		w.Probe.Busy[80] = true
		if conflicts, _ := w.Router.CheckPorts(ctx); len(conflicts) != 1 || conflicts[0].Port != 80 {
			t.Fatalf("CheckPorts = %+v", conflicts)
		}
	})

	t.Run("writes and starts nothing", func(t *testing.T) {
		w := newWorld(t, 80, "")
		stopped(t, w, router.ModeHost)
		before, err := os.ReadFile(w.State.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Router.CheckPorts(ctx); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(w.State.StateFile)
		if string(before) != string(after) || len(w.ups()) != 0 {
			t.Errorf("CheckPorts changed something: state %q -> %q, %d ups", before, after, len(w.ups()))
		}
		if _, err := os.Stat(w.State.ComposeFile); err == nil {
			t.Error("a stack file was written")
		}
	})

	t.Run("reports a daemon treerig refuses", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.info = rootlessInfo
		if _, err := w.Router.CheckPorts(ctx); err == nil {
			t.Fatal("expected the refusal")
		}
	})
}

func TestEnsureReadsTheRouterContainerOnce(t *testing.T) {
	w := newWorld(t, 80, "")
	if err := w.Router.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	var lookups, inspects int
	for _, c := range w.Fake.snapshotCalls() {
		switch {
		case len(c.args) > 0 && c.args[0] == "compose" && containsArg(c.args, "ps") && c.args[len(c.args)-1] == "router":
			lookups++
		case len(c.args) == 2 && c.args[0] == "inspect" && c.args[1] == "router-1":
			inspects++
		}
	}
	if lookups != 1 || inspects != 1 {
		t.Errorf("the router container was looked up %d times and inspected %d times, want once each", lookups, inspects)
	}
}

func TestEnsureStateDropsAnUnusableStoredPort(t *testing.T) {
	w := newWorldOpts(t, "", 80, func(o *router.Options) { o.ModeOverride = "bridge" })
	data, err := json.Marshal(router.State{Version: 1, IndexPort: seedIndexPort, PingPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.State.StateFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Router.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, found, err := router.ReadState(w.State)
	if err != nil || !found {
		t.Fatalf("ReadState = %+v, %v, %v", state, found, err)
	}
	if want := (router.State{Version: 1, IndexPort: seedIndexPort}); state != want {
		t.Errorf("state = %+v, want %+v: the ping port that became the router's own must not stay reserved", state, want)
	}
	if reserved := state.Reserved(); len(reserved) != 1 || reserved[0] != seedIndexPort {
		t.Errorf("reserved = %v", reserved)
	}
}
