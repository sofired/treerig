package cli

// Pins the contract Main and MainProcess rely on to keep `worktree
// remove`'s relocate-and-reexec (internal/commands, #7) from ever
// replacing an in-process test's own image. A go test binary handed a real
// Exec would relocate itself and exec into the copy with argv
// `worktree remove --yes` — no -test.run filter — restarting the whole
// suite in place rather than running the real berth CLI; this is exactly
// how #7's first cut leaked Compose projects through an integration test
// that called Main in-process.
//
// This is checked here, not by actually calling Main end to end, because a
// wrongly-wired Main that really did exec would not fail an assertion — it
// would replace this very test binary's process image before one could run.
// envBuilder is where Main and MainProcess's one-line bodies (cli.go) both
// bottom out, so pinning what it does with the exact values each of them
// passes is a direct, zero-risk stand-in for calling them.

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/logger"
)

func TestEnvBuilderRelocationWiring(t *testing.T) {
	console := logger.NewConsole("berth", io.Discard, io.Discard)
	getenv := func(string) (string, bool) { return "", false }
	args := []string{"berth", "worktree", "remove", "--yes"}

	t.Run("Main's own (nil, nil) builds an Env with no relocation primitives at all", func(t *testing.T) {
		build := envBuilder("berth", "berth worktree remove --yes", console, getenv, strings.NewReader(""), io.Discard, nil, args, nil, nil, nil)
		env, err := build("")
		if err != nil {
			t.Fatalf("build(\"\"): %v", err)
		}
		if env.Executable != nil {
			t.Fatalf("Executable is set; Main must never let worktree remove relocate")
		}
		if env.Exec != nil {
			t.Fatalf("Exec is set; Main must never let worktree remove replace this process")
		}
	})

	t.Run("MainProcess's own (os.Executable, osExec) builds an Env carrying both through", func(t *testing.T) {
		build := envBuilder("berth", "berth worktree remove --yes", console, getenv, strings.NewReader(""), io.Discard, nil, args, os.Executable, osExec, nil)
		env, err := build("")
		if err != nil {
			t.Fatalf("build(\"\"): %v", err)
		}
		if env.Executable == nil {
			t.Fatalf("Executable is nil; MainProcess must hand worktree remove the real one")
		}
		if env.Exec == nil {
			t.Fatalf("Exec is nil; MainProcess must hand worktree remove the real one")
		}
	})
}
