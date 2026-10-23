package commands_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

func TestContextWithoutConfig(t *testing.T) {
	// contextFor builds an Env whose working directory is cwd, with a scratch
	// treerig home, and returns the checkout the context's registry answers for.
	contextFor := func(t *testing.T, cwd, configPath string) string {
		t.Helper()
		home := registrytest.TempDir(t)
		getenv := func(key string) (string, bool) {
			switch key {
			case "TREERIG_HOME":
				return filepath.Join(home, "treerig"), true
			case "HOME":
				return filepath.Join(home, "user"), true
			}
			return "", false
		}
		log := &logger.Recording{}
		env := &commands.Env{
			CommandName: "treerig", Logger: log, Getenv: getenv,
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
		file := filepath.Join(dir, "treerig.toml")
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

func TestContextWithoutConfigRules(t *testing.T) {
	// build returns the context and what it logged for an Env whose working
	// directory is cwd, with a scratch treerig home.
	build := func(t *testing.T, cwd, configPath string) (*commands.CommandContext, *logger.Recording, error) {
		t.Helper()
		env, log := scratchEnv(t, cwd, configPath)
		cctx, err := commands.ContextWithoutConfig(env)
		return cctx, log, err
	}

	t.Run("builds a context with no treerig.toml in reach, and says nothing", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		cctx, log, err := build(t, dir, "")
		if err != nil || cctx.Loaded != nil {
			t.Fatalf("Loaded = %v, err = %v", cctx != nil && cctx.Loaded != nil, err)
		}
		if lines, _ := log.Snapshot(); len(lines) != 0 {
			t.Fatalf("logged %q", lines)
		}
	})

	t.Run("refuses a --config that names a file that is not there, and does not build a context", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		missing := filepath.Join(dir, "nowhere", "treerig.toml")
		cctx, _, err := build(t, dir, missing)
		if err == nil || err.Error() != "No treerig.toml at "+missing+"." || cctx != nil {
			t.Fatalf("cctx = %v, err = %v", cctx, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want it to be a missing file", err)
		}
	})

	t.Run("shared down refuses a --config that is not there instead of going on without it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		missing := filepath.Join(dir, "typo.toml")
		env, log := scratchEnv(t, dir, missing)
		err := commands.SharedDown(context.Background(), env, commands.SharedDownOptions{})
		if err == nil || err.Error() != "No treerig.toml at "+missing+"." {
			t.Fatalf("err = %v", err)
		}
		if lines, _ := log.Snapshot(); len(lines) != 0 {
			t.Fatalf("logged %q", lines)
		}
	})

	t.Run("warns and goes on past a treerig.toml that does not parse, for an explicit --config and a discovered one", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		file := filepath.Join(dir, "treerig.toml")
		if err := os.WriteFile(file, []byte("not = [valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, configPath := range map[string]string{"explicit": file, "discovered": ""} {
			cctx, log, err := build(t, dir, configPath)
			if err != nil || cctx.Loaded != nil {
				t.Fatalf("%s: Loaded = %v, err = %v", name, cctx != nil && cctx.Loaded != nil, err)
			}
			lines, _ := log.Snapshot()
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "warn: Ignoring treerig.toml for this command: "+file+" is not valid TOML: ") {
				t.Fatalf("%s: logged %q", name, lines)
			}
		}
	})

	t.Run("loads a valid treerig.toml, which is what keeps the commands unchanged inside a repository", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		const valid = "[project]\nname = \"fixture\"\ndomain = \"fixture.localhost\"\n\n[compose]\nfile = \"compose.yaml\"\n"
		if err := os.WriteFile(filepath.Join(dir, "treerig.toml"), []byte(valid), 0o644); err != nil {
			t.Fatal(err)
		}
		cctx, log, err := build(t, dir, "")
		if err != nil || cctx.Loaded == nil || cctx.Loaded.Config.Project.Domain != "fixture.localhost" {
			t.Fatalf("Loaded = %+v, err = %v", cctx, err)
		}
		if lines, _ := log.Snapshot(); len(lines) != 0 {
			t.Fatalf("logged %q", lines)
		}
	})
}

// scratchEnv is an Env whose working directory is cwd, with a scratch treerig
// home and a runner that fails the test if anything calls it.
func scratchEnv(t *testing.T, cwd, configPath string) (*commands.Env, *logger.Recording) {
	t.Helper()
	home := registrytest.TempDir(t)
	getenv := func(key string) (string, bool) {
		switch key {
		case "TREERIG_HOME":
			return filepath.Join(home, "treerig"), true
		case "HOME":
			return filepath.Join(home, "user"), true
		}
		return "", false
	}
	log := &logger.Recording{}
	return &commands.Env{
		CommandName: "treerig", Logger: log, Getenv: getenv,
		Cwd: cwd, ConfigPath: configPath, Runner: noRunner{t},
	}, log
}
