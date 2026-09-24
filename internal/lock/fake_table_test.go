package lock

import "github.com/sofired/berth/internal/procid"

// fakeTable is a scripted procid.Table for tests — a Go port of
// tests/unit/lock-recovery.test.ts's FakeTable.
type fakeTable struct {
	identity procid.Identity
	states   map[int]procid.Status
	scans    []procid.Scan
	scanned  int
	inspects []int
	holding  []procid.Proc
}

func newFakeTable(identity procid.Identity) *fakeTable {
	return &fakeTable{identity: identity, states: map[int]procid.Status{}}
}

func (f *fakeTable) Self() (procid.Identity, error) { return f.identity, nil }

func (f *fakeTable) Inspect(pid int) (procid.Status, error) {
	f.inspects = append(f.inspects, pid)
	if s, ok := f.states[pid]; ok {
		return s, nil
	}
	return procid.Status{State: procid.Absent}, nil
}

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

func (f *fakeTable) Holding(dev, ino uint64) ([]procid.Proc, error) { return f.holding, nil }
