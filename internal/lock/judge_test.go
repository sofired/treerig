package lock

import (
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/procid"
)

var self = procid.Identity{MachineID: "machine-1", BootID: "boot-1", PIDNS: "pid:[1]"}

var currentFile = FileID{Dev: 1, Ino: 77}

func testRecord(mutate func(*Record)) *Record {
	r := &Record{
		PID: 4242, Host: "laptop", Command: "berth up",
		StartedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		File:      currentFile,
		MachineID: "machine-1", BootID: "boot-1", PIDNS: "pid:[1]",
	}
	ticks := uint64(900)
	r.StartTicks = &ticks
	if mutate != nil {
		mutate(r)
	}
	return r
}

func mustJudge(t *testing.T, rec *Record, self procid.Identity, table procid.Table, skipFileIdentity bool) Verdict {
	t.Helper()
	v, err := judgeHolder(rec, currentFile, self, "laptop", table, skipFileIdentity)
	if err != nil {
		t.Fatalf("judgeHolder: %v", err)
	}
	return v
}

func TestJudgeHolder(t *testing.T) {
	t.Run("finds a holder gone when its PID no longer exists or has exited", func(t *testing.T) {
		v := mustJudge(t, testRecord(nil), self, newFakeTable(self), false)
		if v.Kind != Gone || v.Reason != `PID 4242 on "laptop" no longer exists` {
			t.Fatalf("got %+v", v)
		}
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Zombie, StartTicks: ptr(uint64(900))}
		v = mustJudge(t, testRecord(nil), self, table, false)
		if v.Kind != Gone || !strings.Contains(v.Reason, "has exited") {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("refuses a holder still running, and names a stopped one as stopped", func(t *testing.T) {
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(900))}
		v := mustJudge(t, testRecord(nil), self, table, false)
		if v.Kind != Alive || v.Reason != `PID 4242 on "laptop" is still running` {
			t.Fatalf("got %+v", v)
		}
		table.states[4242] = procid.Status{State: procid.Stopped, StartTicks: ptr(uint64(900))}
		v = mustJudge(t, testRecord(nil), self, table, false)
		if v.Kind != Alive || !strings.Contains(v.Reason, "`kill -CONT 4242`") {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("sees through a reused PID by its start ticks, and by its start time on macOS", func(t *testing.T) {
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(901))}
		v := mustJudge(t, testRecord(nil), self, table, false)
		if v.Kind != Gone || !strings.Contains(v.Reason, "later process") {
			t.Fatalf("got %+v", v)
		}

		mac := testRecord(func(r *Record) {
			r.StartTicks = nil
			r.PIDNS = ""
			us := int64(10_000_000)
			r.StartUs = &us
		})
		macSelf := procid.Identity{MachineID: "machine-1", BootID: "boot-1"}
		table2 := newFakeTable(macSelf)
		table2.states[4242] = procid.Status{State: procid.Running, StartUs: ptr(int64(11_900_000))}
		v = mustJudge(t, mac, macSelf, table2, false)
		if v.Kind != Alive {
			t.Fatalf("got %+v", v)
		}
		table2.states[4242] = procid.Status{State: procid.Running, StartUs: ptr(int64(12_100_000))}
		v = mustJudge(t, mac, macSelf, table2, false)
		if v.Kind != Gone {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("cannot call a running PID reused without a start to compare, so calls it alive", func(t *testing.T) {
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Running}
		v := mustJudge(t, testRecord(func(r *Record) { r.StartTicks = nil }), self, table, false)
		if v.Kind != Alive {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("finds every holder of an earlier boot gone without asking about its PID", func(t *testing.T) {
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(900))}
		v := mustJudge(t, testRecord(func(r *Record) { r.BootID = "boot-0" }), self, table, false)
		if v.Kind != Gone || !strings.Contains(v.Reason, "restarted") {
			t.Fatalf("got %+v", v)
		}
		if len(table.inspects) != 0 {
			t.Fatalf("expected no Inspect calls, got %v", table.inspects)
		}
	})

	t.Run("cannot check a holder on another machine, in another namespace, or with no record — and says why", func(t *testing.T) {
		cases := []struct {
			rec    *Record
			self   procid.Identity
			reason string
		}{
			{testRecord(func(r *Record) { r.MachineID = "machine-2" }), self, "another machine"},
			{testRecord(func(r *Record) { r.PIDNS = "pid:[9]" }), self, "another PID namespace"},
			{testRecord(func(r *Record) { r.PIDNS = "" }), self, "another PID namespace"},
			{testRecord(nil), procid.Identity{PIDNS: "pid:[1]"}, "this machine's identity cannot be read"},
			{testRecord(func(r *Record) { r.BootID = "" }), procid.Identity{BootID: "boot-1", PIDNS: "pid:[1]"}, "this machine's identity cannot be read"},
			{testRecord(func(r *Record) { r.MachineID = ""; r.BootID = "boot-0" }), self, "with no machine ID to go on"},
			{nil, self, "left no record"},
		}
		for _, c := range cases {
			v := mustJudge(t, c.rec, c.self, newFakeTable(self), false)
			if v.Kind != Unverifiable || !strings.Contains(v.Reason, c.reason) {
				t.Errorf("rec=%+v: got %+v, want reason containing %q", c.rec, v, c.reason)
			}
		}
	})

	t.Run("treats a record naming a different file identity as belonging to an earlier lock", func(t *testing.T) {
		v := mustJudge(t, testRecord(func(r *Record) { r.File = FileID{Dev: 1, Ino: 78} }), self, newFakeTable(self), false)
		if v.Kind != Unverifiable || !strings.Contains(v.Reason, "belongs to an earlier lock") {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("knows this machine by its boot where either side has no machine ID, as in a container image without one", func(t *testing.T) {
		noMachineID := procid.Identity{BootID: "boot-1", PIDNS: "pid:[1]"}
		v := mustJudge(t, testRecord(nil), noMachineID, newFakeTable(self), false)
		if v.Kind != Gone {
			t.Fatalf("got %+v", v)
		}
		v = mustJudge(t, testRecord(func(r *Record) { r.MachineID = "" }), self, newFakeTable(self), false)
		if v.Kind != Gone {
			t.Fatalf("got %+v", v)
		}
		table := newFakeTable(noMachineID)
		table.states[4242] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(900))}
		v = mustJudge(t, testRecord(func(r *Record) { r.MachineID = "" }), noMachineID, table, false)
		if v.Kind != Alive {
			t.Fatalf("got %+v", v)
		}
		// The same kernel is not the same PID namespace: another container on it still cannot be checked.
		v = mustJudge(t, testRecord(func(r *Record) { r.MachineID = ""; r.PIDNS = "pid:[9]" }), noMachineID, table, false)
		if v.Kind != Unverifiable || !strings.Contains(v.Reason, "another PID namespace") {
			t.Fatalf("got %+v", v)
		}
	})

	t.Run("goes by hostname alone for a record an earlier version wrote, and never proves that holder gone", func(t *testing.T) {
		legacy := func(mutate func(*Record)) *Record {
			r := &Record{PID: 4242, Host: "laptop", Command: "", StartedAt: time.Now()}
			if mutate != nil {
				mutate(r)
			}
			return r
		}
		v := mustJudge(t, legacy(func(r *Record) { r.Host = "desktop" }), self, newFakeTable(self), true)
		if v.Kind != Unverifiable || !strings.Contains(v.Reason, "another host") {
			t.Fatalf("got %+v", v)
		}
		v = mustJudge(t, legacy(nil), self, newFakeTable(self), true)
		if v.Kind != Unverifiable {
			t.Fatalf("got %+v", v)
		}
		table := newFakeTable(self)
		table.states[4242] = procid.Status{State: procid.Zombie}
		v = mustJudge(t, legacy(nil), self, table, true)
		if v.Kind != Unverifiable {
			t.Fatalf("got %+v", v)
		}
		table.states[4242] = procid.Status{State: procid.Running}
		v = mustJudge(t, legacy(nil), self, table, true)
		if v.Kind != Alive {
			t.Fatalf("got %+v", v)
		}
	})
}

func ptr[T any](v T) *T { return &v }

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{4_000 * time.Millisecond, "4s"},
		{750_000 * time.Millisecond, "12m 30s"},
		{7_500_000 * time.Millisecond, "2h 5m"},
		{200_000_000 * time.Millisecond, "2d 7h"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
