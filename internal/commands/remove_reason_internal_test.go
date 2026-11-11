package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
)

func TestCommandReason(t *testing.T) {
	t.Run("keeps Git's reason when a long directory fills the command line", func(t *testing.T) {
		dir := "/private/var/folders/n6/" + strings.Repeat("x", 120) + "/outside"
		err := run.Failure(run.Spec{Command: "git", Args: []string{"-C", dir, "rev-parse", "--show-toplevel"}},
			run.Result{ExitCode: 128, Stderr: "fatal: detected dubious ownership in repository at '" + dir + "'"})
		if got := text.OneLine(commandReason(err), 200); !strings.HasPrefix(got, "fatal: detected dubious ownership") {
			t.Fatalf("reason = %q", got)
		}
	})

	t.Run("leaves any other error as it is", func(t *testing.T) {
		if got := commandReason(errors.New("Git is not installed")); got != "Git is not installed" {
			t.Fatalf("reason = %q", got)
		}
	})
}
