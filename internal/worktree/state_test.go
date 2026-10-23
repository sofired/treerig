package worktree_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/worktree"
)

// kind is one service condition the mapping test combines: a service state,
// and for an exit which code.
type kind struct {
	name   string
	status worktree.ServiceStatus
}

// kinds is every condition a service can be in, as far as Classify tells them
// apart: the eight service states, with an exit split by whether it was clean,
// and two states nobody has a word for (one added later, and the zero value).
var kinds = []kind{
	{"running", worktree.ServiceStatus{State: worktree.Running}},
	{"starting", worktree.ServiceStatus{State: worktree.Starting}},
	{"unhealthy", worktree.ServiceStatus{State: worktree.Unhealthy}},
	{"restarting", worktree.ServiceStatus{State: worktree.Restarting}},
	{"exited 0", worktree.ServiceStatus{State: worktree.Exited}},
	{"exited 3", worktree.ServiceStatus{State: worktree.Exited, ExitCode: 3}},
	{"stopped", worktree.ServiceStatus{State: worktree.Stopped}},
	{"not created", worktree.ServiceStatus{State: worktree.NotCreated}},
	{"unknown", worktree.ServiceStatus{State: worktree.Unknown, Detail: "mystery"}},
	{"unknown", worktree.ServiceStatus{State: "levitating"}},
	{"unknown", worktree.ServiceStatus{}},
}

// expected decides a worktree's state from how many services are in each
// condition, written out from the design's table (docs/design/cli-1.0.md,
// "State words") and not by looking at the services one at a time as Classify
// does, so the two can only agree by both being right.
func expected(counts map[string]int, started bool) worktree.State {
	bad := counts["unhealthy"] + counts["restarting"] + counts["exited 3"]
	idle := counts["stopped"] + counts["not created"]
	switch {
	case counts["unknown"] > 0:
		return worktree.StateUnknown
	case counts["starting"] > 0:
		return worktree.StateStarting
	case counts["running"] == 0 && bad > 0:
		return worktree.StateFailed
	case counts["running"] > 0 && bad+idle > 0:
		return worktree.StatePartlyRunning
	case counts["running"] > 0:
		return worktree.StateRunning
	case started:
		return worktree.StateStopped
	}
	return worktree.StateNotStarted
}

func TestClassifyMapsEveryCombinationToOneState(t *testing.T) {
	t.Run("maps every combination of up to three service states, with and without a finished up, to exactly one state", func(t *testing.T) {
		valid := map[worktree.State]bool{}
		for _, state := range worktree.States {
			valid[state] = true
		}
		checked := 0
		var walk func(prefix []kind)
		walk = func(prefix []kind) {
			counts := map[string]int{}
			services := make([]worktree.ServiceStatus, len(prefix))
			names := make([]string, len(prefix))
			for i, k := range prefix {
				counts[k.name]++
				services[i] = k.status
				names[i] = k.name
			}
			for _, started := range []bool{false, true} {
				got := worktree.Classify(worktree.Inputs{Directory: worktree.DirectoryPresent, Started: started, Services: services})
				want := expected(counts, started)
				label := fmt.Sprintf("[%s] started=%v", strings.Join(names, ", "), started)
				if !valid[got.State] {
					t.Fatalf("%s: %q is not one of the nine states", label, got.State)
				}
				if got.State != want {
					t.Fatalf("%s: got %q, want %q", label, got.State, want)
				}
				if (got.State == worktree.StateUnknown) != (got.Hint != "") {
					t.Fatalf("%s: state %q with hint %q: an unknown state names its cause and no other does", label, got.State, got.Hint)
				}
				checked++
			}
			if len(prefix) == 3 {
				return
			}
			for _, k := range kinds {
				walk(append(append([]kind(nil), prefix...), k))
			}
		}
		walk(nil)
		if want := 2 * (1 + 11 + 121 + 1331); checked != want {
			t.Fatalf("checked %d combinations, want %d", checked, want)
		}
	})

	t.Run("gives the design's examples", func(t *testing.T) {
		running := worktree.ServiceStatus{Service: "web", State: worktree.Running}
		migrated := worktree.ServiceStatus{Service: "migrate", State: worktree.Exited}
		crashed := worktree.ServiceStatus{Service: "worker", State: worktree.Exited, ExitCode: 3}
		for _, c := range []struct {
			name     string
			started  bool
			services []worktree.ServiceStatus
			want     worktree.State
		}{
			{"web running beside a finished migrate", true, []worktree.ServiceStatus{running, migrated}, worktree.StateRunning},
			{"every service exited cleanly", true, []worktree.ServiceStatus{migrated, {Service: "seed", State: worktree.Exited}}, worktree.StateStopped},
			{"a first up that fails with one service running", false, []worktree.ServiceStatus{running, crashed}, worktree.StatePartlyRunning},
			{"a first up whose only service crashed", false, []worktree.ServiceStatus{crashed}, worktree.StateFailed},
			{"after down", true, []worktree.ServiceStatus{{Service: "web", State: worktree.NotCreated}}, worktree.StateStopped},
			{"registered and never up", false, []worktree.ServiceStatus{{Service: "web", State: worktree.NotCreated}}, worktree.StateNotStarted},
			{"no services declared, up finished", true, nil, worktree.StateStopped},
			{"a service starting beside one crashed", true, []worktree.ServiceStatus{{State: worktree.Starting}, crashed}, worktree.StateStarting},
		} {
			got := worktree.Classify(worktree.Inputs{Directory: worktree.DirectoryPresent, Started: c.started, Services: c.services})
			if got.State != c.want {
				t.Fatalf("%s: got %q, want %q", c.name, got.State, c.want)
			}
		}
	})

	t.Run("applies removing, directory missing and unknown before the containers, in that order", func(t *testing.T) {
		running := []worktree.ServiceStatus{{Service: "web", State: worktree.Running}}
		for _, c := range []struct {
			name string
			in   worktree.Inputs
			want worktree.State
		}{
			{"removing beats everything", worktree.Inputs{Removing: true, Directory: worktree.DirectoryGone, ServicesHint: "down", Services: running}, worktree.StateRemoving},
			{"directory missing beats unknown and the containers", worktree.Inputs{Directory: worktree.DirectoryGone, ServicesHint: "down", Services: running}, worktree.StateDirectoryMissing},
			{"an undetermined directory is unknown", worktree.Inputs{Directory: worktree.DirectoryUndetermined, DirectoryHint: "why", Services: running}, worktree.StateUnknown},
			{"Docker not answering is unknown", worktree.Inputs{Directory: worktree.DirectoryPresent, ServicesHint: "down", Services: running}, worktree.StateUnknown},
			{"a directory nobody asked about says nothing", worktree.Inputs{Directory: worktree.DirectoryNotChecked, Services: running}, worktree.StateRunning},
		} {
			if got := worktree.Classify(c.in); got.State != c.want {
				t.Fatalf("%s: got %q, want %q", c.name, got.State, c.want)
			}
		}
	})

	t.Run("names the cause of an unknown state", func(t *testing.T) {
		hint := worktree.Classify(worktree.Inputs{Directory: worktree.DirectoryUndetermined, DirectoryHint: "Its parent directory is missing or empty: is the volume mounted?"}).Hint
		if hint != "Its parent directory is missing or empty: is the volume mounted?" {
			t.Fatalf("directory hint = %q", hint)
		}
		if hint := worktree.Classify(worktree.Inputs{ServicesHint: "Docker didn't answer: Cannot connect"}).Hint; hint != "Docker didn't answer: Cannot connect" {
			t.Fatalf("docker hint = %q", hint)
		}
		hint = worktree.Classify(worktree.Inputs{Services: []worktree.ServiceStatus{{Service: "web", State: worktree.Unknown, Detail: "mystery"}}}).Hint
		if hint != "web is in a state treerig has no word for (Docker says mystery)." {
			t.Fatalf("service hint = %q", hint)
		}
		hint = worktree.Classify(worktree.Inputs{Services: []worktree.ServiceStatus{{Service: "web", State: "levitating"}}}).Hint
		if hint != "web is in a state treerig has no word for (levitating)." {
			t.Fatalf("unrecognised state hint = %q", hint)
		}
		hint = worktree.Classify(worktree.Inputs{Services: []worktree.ServiceStatus{{Service: "web"}}}).Hint
		if hint != "web is in a state treerig has no word for." {
			t.Fatalf("zero state hint = %q", hint)
		}
		hint = worktree.Classify(worktree.Inputs{Services: []worktree.ServiceStatus{{Service: "job", State: worktree.Unknown, Detail: worktree.CauseNotRead}}}).Hint
		if hint != "Whether job was stopped or killed for running out of memory (it exited 137) couldn't be read." {
			t.Fatalf("cause hint = %q", hint)
		}
		hint = worktree.Classify(worktree.Inputs{Services: []worktree.ServiceStatus{{Service: "job", State: worktree.Unknown, Detail: "exited"}}}).Hint
		if hint != "No exit code was reported for job, which exited." {
			t.Fatalf("exit code hint = %q", hint)
		}
	})
}

func TestStateWordsMapToJSON(t *testing.T) {
	t.Run("maps every worktree state word to one snake case JSON value", func(t *testing.T) {
		want := map[worktree.State]string{
			worktree.StateRemoving:         "removing",
			worktree.StateDirectoryMissing: "directory_missing",
			worktree.StateUnknown:          "unknown",
			worktree.StateStarting:         "starting",
			worktree.StateFailed:           "failed",
			worktree.StatePartlyRunning:    "partly_running",
			worktree.StateRunning:          "running",
			worktree.StateStopped:          "stopped",
			worktree.StateNotStarted:       "not_started",
		}
		if len(want) != len(worktree.States) {
			t.Fatalf("the table has %d states and States lists %d", len(want), len(worktree.States))
		}
		seen := map[string]bool{}
		for _, state := range worktree.States {
			json, ok := want[state]
			if !ok {
				t.Fatalf("%q has no row in the table", state)
			}
			if got := state.JSON(); got != json {
				t.Fatalf("%q: JSON = %q, want %q", state, got, json)
			}
			if seen[json] {
				t.Fatalf("%q is the JSON value of two states", json)
			}
			seen[json] = true
		}
		if got := worktree.States[1].String(); got != "directory missing" {
			t.Fatalf("text = %q", got)
		}
	})

	t.Run("writes and reads the JSON spelling when a document holds a state", func(t *testing.T) {
		type document struct {
			State   worktree.State        `json:"state"`
			Service worktree.ServiceState `json:"service"`
		}
		data, err := json.Marshal(document{State: worktree.StatePartlyRunning, Service: worktree.NotCreated})
		if err != nil || string(data) != `{"state":"partly_running","service":"not_created"}` {
			t.Fatalf("Marshal = %s, %v", data, err)
		}
		var back document
		if err := json.Unmarshal(data, &back); err != nil || back.State != worktree.StatePartlyRunning || back.Service != worktree.NotCreated {
			t.Fatalf("Unmarshal = %+v, %v", back, err)
		}
		if err := json.Unmarshal([]byte(`{"state":"partly running"}`), &back); err == nil {
			t.Fatal("the text word is not the JSON spelling and must not be read as one")
		}
	})

	t.Run("maps every service state word to one snake case JSON value", func(t *testing.T) {
		want := map[worktree.ServiceState]string{
			worktree.Running:    "running",
			worktree.Starting:   "starting",
			worktree.Unhealthy:  "unhealthy",
			worktree.Restarting: "restarting",
			worktree.Exited:     "exited",
			worktree.Stopped:    "stopped",
			worktree.NotCreated: "not_created",
			worktree.Unknown:    "unknown",
		}
		if len(want) != len(worktree.ServiceStates) {
			t.Fatalf("the table has %d states and ServiceStates lists %d", len(want), len(worktree.ServiceStates))
		}
		for _, state := range worktree.ServiceStates {
			if got := state.JSON(); got != want[state] {
				t.Fatalf("%q: JSON = %q, want %q", state, got, want[state])
			}
		}
	})
}

func TestContainerStatus(t *testing.T) {
	for _, c := range []struct {
		name          string
		state, health string
		code          int
		known         bool
		want          worktree.ServiceStatus
	}{
		{"running", "running", "", 0, false, worktree.ServiceStatus{State: worktree.Running}},
		{"running and healthy", "running", "healthy", 0, false, worktree.ServiceStatus{State: worktree.Running}},
		{"running with its health check pending", "running", "starting", 0, false, worktree.ServiceStatus{State: worktree.Starting}},
		{"running and failing its health check", "running", "unhealthy", 0, false, worktree.ServiceStatus{State: worktree.Unhealthy}},
		{"restarting", "restarting", "", 0, false, worktree.ServiceStatus{State: worktree.Restarting}},
		{"exited with a code", "exited", "", 3, true, worktree.ServiceStatus{State: worktree.Exited, ExitCode: 3}},
		{"exited cleanly", "Exited", "", 0, true, worktree.ServiceStatus{State: worktree.Exited}},
		{"exited with no code known", "exited", "", 0, false, worktree.ServiceStatus{State: worktree.Unknown, Detail: "exited"}},
		{"created", "created", "", 0, false, worktree.ServiceStatus{State: worktree.Stopped}},
		{"killed by SIGKILL, with the cause not read", "exited", "", 137, true, worktree.ServiceStatus{State: worktree.Unknown, Detail: worktree.CauseNotRead}},
		{"ended by SIGTERM", "exited", "", 143, true, worktree.ServiceStatus{State: worktree.Stopped}},
		{"a paused one", "paused", "", 0, false, worktree.ServiceStatus{State: worktree.Stopped}},
		{"dead", "dead", "", 0, false, worktree.ServiceStatus{State: worktree.Stopped}},
		{"being removed", "removing", "", 0, false, worktree.ServiceStatus{State: worktree.Stopped}},
		{"a word Docker adds later", "frozen", "", 0, false, worktree.ServiceStatus{State: worktree.Unknown, Detail: "frozen"}},
	} {
		t.Run("reads "+c.name, func(t *testing.T) {
			if got := worktree.ContainerStatus(c.state, c.health, c.code, c.known, false, false); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestContainerStatusOutOfMemory(t *testing.T) {
	for _, c := range []struct {
		name      string
		code      int
		oom, read bool
		want      worktree.ServiceStatus
	}{
		{"a stop by SIGKILL that Docker says the out-of-memory killer did not do", 137, false, true, worktree.ServiceStatus{State: worktree.Stopped}},
		{"a kill that Docker says the out-of-memory killer did", 137, true, true, worktree.ServiceStatus{State: worktree.Exited, ExitCode: 137}},
		{"a 137 nobody asked Docker about", 137, false, false, worktree.ServiceStatus{State: worktree.Unknown, Detail: worktree.CauseNotRead}},
		{"a SIGTERM exit, never the out-of-memory killer's", 143, true, true, worktree.ServiceStatus{State: worktree.Stopped}},
		{"a SIGTERM exit nobody asked about", 143, false, false, worktree.ServiceStatus{State: worktree.Stopped}},
		{"another code, whatever was asked", 3, false, false, worktree.ServiceStatus{State: worktree.Exited, ExitCode: 3}},
	} {
		t.Run("reads "+c.name, func(t *testing.T) {
			if got := worktree.ContainerStatus("exited", "", c.code, true, c.oom, c.read); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	t.Run("shows a scaled service as its worst replica", func(t *testing.T) {
		got := worktree.Summarize([]worktree.ServiceStatus{
			{Service: "web", State: worktree.Running},
			{Service: "api", State: worktree.Running},
			{Service: "web", State: worktree.Exited, ExitCode: 1},
			{Service: "web", State: worktree.Running},
			{Service: "job", State: worktree.Exited},
			{Service: "job", State: worktree.NotCreated},
		})
		want := []worktree.ServiceStatus{
			{Service: "web", State: worktree.Exited, ExitCode: 1},
			{Service: "api", State: worktree.Running},
			{Service: "job", State: worktree.NotCreated},
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("has nothing to say for a worktree with no containers", func(t *testing.T) {
		if got := worktree.Summarize(nil); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("prints an exit with its code", func(t *testing.T) {
		if got := (worktree.ServiceStatus{State: worktree.Exited, ExitCode: 3}).Text(); got != "exited (code 3)" {
			t.Fatalf("Text = %q", got)
		}
		if got := (worktree.ServiceStatus{State: worktree.NotCreated}).Text(); got != "not created" {
			t.Fatalf("Text = %q", got)
		}
	})
}
