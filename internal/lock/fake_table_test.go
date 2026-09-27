package lock

import "github.com/sofired/berth/internal/procid"

// fakeTable is a scripted procid.Table for tests.
type fakeTable struct {
	identity procid.Identity
	states   map[int]procid.Status
	scans    []procid.Scan
	scanned  int
	inspects []int
	holding  []procid.Proc
}

// newFakeTable builds a fakeTable that reports identity as Self and Absent
// for any PID not later added to states.
func newFakeTable(identity procid.Identity) *fakeTable {
	return &fakeTable{identity: identity, states: map[int]procid.Status{}}
}

// Self returns the identity newFakeTable was built with.
func (f *fakeTable) Self() (procid.Identity, error) { return f.identity, nil }

// Inspect records pid in f.inspects and returns its scripted status, or
// Absent if the test never set one.
func (f *fakeTable) Inspect(pid int) (procid.Status, error) {
	f.inspects = append(f.inspects, pid)
	if s, ok := f.states[pid]; ok {
		return s, nil
	}
	return procid.Status{State: procid.Absent}, nil
}

// Scan returns the next scripted scan in f.scans, holding on the last one
// once exhausted, or an empty Scan if none were scripted at all.
func (f *fakeTable) Scan() (procid.Scan, error) {
	if len(f.scans) == 0 {
		f.scanned++
		return procid.Scan{}, nil
	}
	idx := f.scanned
	if idx >= len(f.scans) {
		idx = len(f.scans) - 1
	}
	f.scanned++
	return f.scans[idx], nil
}

// Holding returns f.holding regardless of dev and ino.
func (f *fakeTable) Holding(dev, ino uint64) ([]procid.Proc, error) { return f.holding, nil }
