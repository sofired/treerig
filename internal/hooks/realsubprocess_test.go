package hooks_test

// A real subprocess test, not gated behind -tags integration: it only spawns
// a local script, the same kind of operation
// internal/run/osrun/osrun_test.go's own "tells a script without a #! line
// what it is missing" already runs as a plain unit test.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/worktree"
)

// TestRunHookOverARealSubprocess pins divergence 6c end to end, through the
// real hooks runner over the real run.Runner (osrun), not a fake: Go's
// exec has no shell fallback for a script missing its interpreter line,
// where Node's execvp falls back to /bin/sh, so a hook with no #! line
// fails to start rather than running under a shell.
func TestRunHookOverARealSubprocess(t *testing.T) {
	t.Run("a hook with no #! line fails to start: exit 126, ENOEXEC", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "prepare")
		if err := os.WriteFile(script, []byte("not a script\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		hctx := hooks.HookContext{
			Source: worktree.EnvironmentSource{ProjectRoot: dir},
			Ref:    worktree.Ref{RepositoryID: "repo-a", WorktreeID: "wt-1", Slug: "feature-login"},
			Hooks:  config.Hooks{Prepare: &script},
			Logger: logger.Silent{},
			Runner: osrun.New(),
		}
		ran, err := hooks.RunHook(context.Background(), config.Prepare, hctx, hooks.RunOptions{})
		if ran {
			t.Fatalf("expected the hook not to report having run")
		}
		want := fmt.Sprintf(
			"The prepare hook could not be started: `%s` is not a valid executable (ENOEXEC) — a script needs a #! line.",
			script,
		)
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
		if code := clierr.Code(err); code != 126 {
			t.Fatalf("got exit code %d, want 126", code)
		}
	})
}
