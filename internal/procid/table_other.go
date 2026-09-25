//go:build !linux && !darwin

package procid

import (
	"errors"
	"runtime"

	"github.com/sofired/berth/internal/clierr"
)

// UnsupportedTable refuses rather than guesses, on every platform this tool
// does not support. There is no Windows build; see the design doc.
type UnsupportedTable struct{}

// Platform is this platform's process table.
func Platform() Table { return UnsupportedTable{} }

// Self reports an empty identity: there is nothing this platform can prove
// about itself, but a lock can still be taken with a blank identity recorded.
func (UnsupportedTable) Self() (Identity, error) { return Identity{}, nil }

// Inspect always refuses: this platform has no way to check a PID.
func (UnsupportedTable) Inspect(int) (Status, error) {
	return Status{}, clierr.Newf("processes cannot be checked on %s", runtime.GOOS)
}

// Scan always refuses: this platform has no way to list processes.
func (UnsupportedTable) Scan() (Scan, error) {
	return Scan{}, clierr.Newf("processes cannot be listed on %s", runtime.GOOS)
}

// Holding always refuses: this platform has no way to enumerate a file's open descriptors.
func (UnsupportedTable) Holding(uint64, uint64) ([]Proc, error) {
	return nil, errors.ErrUnsupported
}
