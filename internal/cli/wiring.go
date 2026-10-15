package cli

import (
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/osrun"
)

// init wires newRunner to the real osrun.Runner, so production commands run
// actual subprocesses; tests overwrite newRunner themselves and never see
// this default.
func init() {
	newRunner = func() run.Runner { return osrun.New() }
}
