package cli

import (
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/osrun"
)

func init() {
	newRunner = func() run.Runner { return osrun.New() }
}
