package lock

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/procid"
)

// TestSurvivorRefusalLayout pins the whole shape of the two refusals that
// list the processes a gone holder left running: one indented line per
// process, then the advice on a line of its own, so that no punctuation is
// read as part of the last process's command line. Real time (judgeDelay's
// rescan): about a second each.
func TestSurvivorRefusalLayout(t *testing.T) {
	survivors := []procid.Proc{
		{PID: 4242, Args: []string{"berth-router", "sleep", "1"}},
		{PID: 4243, Args: []string{"berth-router", "sleep", "2"}},
	}
	scope := func(t *testing.T) Scope {
		t.Helper()
		return Scope{Target: filepath.Join(t.TempDir(), "router"), Description: "shared router", Markers: procid.Markers{Project: "berth-router"}}
	}
	abandoned := func(t *testing.T, sc Scope) {
		t.Helper()
		lk := New(sc, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("taking the lock lists the survivors, then the advice on its own line", func(t *testing.T) {
		sc := scope(t)
		abandoned(t, sc)
		opts := testOptions(t)
		opts.Table.(*fakeTable).scans = []procid.Scan{{Procs: survivors}}
		_, err := New(sc, opts).Acquire(context.Background(), "berth up")
		want := "Not taking the shared router lock: its previous holder is gone, but 2 processes it started are still running:\n" +
			"  PID 4242: berth-router sleep 1\n" +
			"  PID 4243: berth-router sleep 2\n" +
			"Let them finish or stop them, then run this again."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %q, want %q", err, want)
		}
	})

	t.Run("clearing the lock lists the survivors, then the advice on its own line", func(t *testing.T) {
		sc := scope(t)
		abandoned(t, sc)
		opts := clearOptions(t)
		opts.Table.(*fakeTable).scans = []procid.Scan{{Procs: survivors[:1]}}
		_, err := Clear(context.Background(), []Scope{sc}, opts)
		want := "Not clearing any lock: a process it started is still running:\n" +
			"  PID 4242: berth-router sleep 1\n" +
			"Let it finish or stop it, then run this again."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %q, want %q", err, want)
		}
	})

	t.Run("a long list is cut at ten, and the advice still follows on its own line", func(t *testing.T) {
		sc := scope(t)
		abandoned(t, sc)
		opts := testOptions(t)
		var many []procid.Proc
		want := "Not taking the shared router lock: its previous holder is gone, but 12 processes it started are still running:\n"
		for i := 0; i < 12; i++ {
			many = append(many, procid.Proc{PID: 5000 + i, Args: []string{"berth-router", "sleep"}})
			if i < 10 {
				want += fmt.Sprintf("  PID %d: berth-router sleep\n", 5000+i)
			}
		}
		want += "  … and 2 more\nLet them finish or stop them, then run this again."
		opts.Table.(*fakeTable).scans = []procid.Scan{{Procs: many}}
		_, err := New(sc, opts).Acquire(context.Background(), "berth up")
		if err == nil || err.Error() != want {
			t.Fatalf("err = %q, want %q", err, want)
		}
	})
}
