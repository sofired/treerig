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

func (UnsupportedTable) Self() (Identity, error) { return Identity{}, nil }

func (UnsupportedTable) Inspect(int) (Status, error) {
	return Status{}, clierr.Newf("processes cannot be checked on %s", runtime.GOOS)
}

func (UnsupportedTable) Scan() (Scan, error) {
	return Scan{}, clierr.Newf("processes cannot be listed on %s", runtime.GOOS)
}

func (UnsupportedTable) Holding(uint64, uint64) ([]Proc, error) {
	return nil, errors.ErrUnsupported
}
