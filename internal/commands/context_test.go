package commands_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
)

func TestContextWithoutConfig(t *testing.T) {
	// contextFor builds an Env whose working directory is cwd, with a scratch
	// berth home, and returns the checkout the context's registry answers for.
	contextFor := func(t *testing.T, cwd, configPath string) string {
		t.Helper()
		home := registrytest.TempDir(t)
		getenv := func(key string) (string, bool) {
			switch key {
			case "BERTH_HOME":
				return filepath.Join(home, "berth"), true
			case "HOME":
				return filepath.Join(home, "user"), true
			}
			return "", false
		}
		log := &logger.Recording{}
		env := &commands.Env{
			CommandName: "berth", Logger: log, Getenv: getenv,
			Cwd: cwd, ConfigPath: configPath, Runner: noRunner{t},
		}
		cctx, err := commands.ContextWithoutConfig(env)
		if err != nil {
			t.Fatal(err)
		}
		if cctx.Loaded != nil {
			t.Fatal("an invalid config was loaded")
		}
		return cctx.Registry.Identities.Root
	}
	invalid := func(t *testing.T, dir string) string {
		t.Helper()
		file := filepath.Join(dir, "berth.toml")
		if err := os.WriteFile(file, []byte("not = [valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}

	t.Run("an explicit config that fails to load still names its own directory, not the working directory's repository", func(t *testing.T) {
		other, mine := registrytest.TempDir(t), registrytest.TempDir(t)
		file := invalid(t, other)
		if got := contextFor(t, mine, file); got != other {
			t.Fatalf("root = %s, want %s", got, other)
		}
	})

	t.Run("an explicit config that does not exist names its own directory, not the working directory's repository", func(t *testing.T) {
		other, mine := registrytest.TempDir(t), registrytest.TempDir(t)
		if got := contextFor(t, mine, filepath.Join(other, "berth.toml")); got != other {
			t.Fatalf("root = %s, want %s", got, other)
		}
	})

	t.Run("a discovered config that fails to load names the directory it was found in", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		invalid(t, dir)
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := contextFor(t, sub, ""); got != dir {
			t.Fatalf("root = %s, want %s", got, dir)
		}
	})

	t.Run("with no config at all it answers for the working directory", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		if got := contextFor(t, dir, ""); got != dir {
			t.Fatalf("root = %s, want %s", got, dir)
		}
	})
}
