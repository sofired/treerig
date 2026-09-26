package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/logger"
)

const minimalTOML = `
[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"

[hooks]
prepare = "./prepare.sh"
backup = "./backup.sh"
`

// testEnv builds an Env pointed at configPath, with Getenv resolving only
// HOME (to home) and Cwd set to configPath's directory, plus the Recording
// logger it writes to.
func testEnv(t *testing.T, configPath, home string) (*Env, *logger.Recording) {
	t.Helper()
	rec := &logger.Recording{}
	getenv := func(key string) (string, bool) {
		if key == "HOME" {
			return home, true
		}
		return "", false
	}
	return &Env{
		CommandName: "berth",
		Logger:      rec,
		Getenv:      getenv,
		Cwd:         filepath.Dir(configPath),
		ConfigPath:  configPath,
	}, rec
}

// writeConfig writes toml to a fresh temp directory's berth.toml and returns
// its path.
func writeConfig(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "berth.toml")
	if err := os.WriteFile(file, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestConfigValidate(t *testing.T) {
	file := writeConfig(t, minimalTOML)
	home := t.TempDir()
	env, rec := testEnv(t, file, home)

	if err := ConfigValidate(context.Background(), env); err != nil {
		t.Fatalf("ConfigValidate: %v", err)
	}

	_, printed := rec.Snapshot()
	if len(printed) != 4 {
		t.Fatalf("Output lines = %d, want 4: %#v", len(printed), printed)
	}
	if got, want := printed[0], file+" is valid."; got != want {
		t.Errorf("line 0 = %q, want %q", got, want)
	}
	if want := "project acme · domain acme.localhost · 0 service(s) · 1 hook(s)"; printed[1] != want {
		t.Errorf("line 1 = %q, want %q", printed[1], want)
	}
	// prepare is invoked; backup is accepted but not invoked yet.
	if want := "accepted but not invoked yet: backup"; printed[2] != want {
		t.Errorf("line 2 = %q, want %q", printed[2], want)
	}
	if want := "control home: " + filepath.Join(home, ".berth", "control-home"); printed[3] != want {
		t.Errorf("line 3 = %q, want %q", printed[3], want)
	}
}

func TestConfigValidateNoInertHooksLine(t *testing.T) {
	file := writeConfig(t, minimalTOML+"\nverify = \"./verify.sh\"\n")
	env, rec := testEnv(t, file, t.TempDir())

	if err := ConfigValidate(context.Background(), env); err != nil {
		t.Fatalf("ConfigValidate: %v", err)
	}
	_, printed := rec.Snapshot()
	for _, line := range printed {
		if strings.HasPrefix(line, "accepted but not invoked yet") && !strings.Contains(line, "backup") {
			t.Errorf("expected backup still listed as inert: %q", line)
		}
	}
}

func TestConfigValidatePropagatesLoadError(t *testing.T) {
	env, _ := testEnv(t, filepath.Join(t.TempDir(), "missing.toml"), t.TempDir())
	if err := ConfigValidate(context.Background(), env); err == nil {
		t.Fatal("ConfigValidate: expected an error for a missing config file")
	}
}

// `config validate` answers "would up work?", so it refuses exactly what
// `up` itself refuses: a control character in a [service.index] label or
// purpose (#19, row 25 in docs/port/divergences.md). The base loader that
// backs `load` stays lenient — down, worktree remove, status and the rest
// load the same file and proceed — so this has to be its own check, not
// something the load above already caught.
func TestConfigValidateRefusesControlCharacters(t *testing.T) {
	file := writeConfig(t, minimalTOML+"[[service]]\nname = \"api\"\n[service.index]\nlabel = \"a\tb\"\n")
	env, rec := testEnv(t, file, t.TempDir())

	err := ConfigValidate(context.Background(), env)
	want := file + " is not a valid berth.toml:\n  service.0.index.label: must not contain control characters"
	if err == nil || err.Error() != want {
		t.Fatalf("ConfigValidate error = %v, want %q", err, want)
	}
	if _, printed := rec.Snapshot(); len(printed) != 0 {
		t.Fatalf("printed = %#v, want nothing: a refusal prints no report", printed)
	}
}

func TestConfigShow(t *testing.T) {
	file := writeConfig(t, minimalTOML)
	env, rec := testEnv(t, file, t.TempDir())

	if err := ConfigShow(context.Background(), env); err != nil {
		t.Fatalf("ConfigShow: %v", err)
	}

	_, printed := rec.Snapshot()
	if len(printed) != 1 {
		t.Fatalf("Output lines = %d, want 1: %#v", len(printed), printed)
	}
	want := `{
  "project": {
    "name": "acme",
    "domain": "acme.localhost"
  },
  "compose": {
    "file": "docker/compose.yaml"
  },
  "service": [],
  "hooks": {
    "prepare": "./prepare.sh",
    "backup": "./backup.sh"
  }
}`
	if printed[0] != want {
		t.Errorf("ConfigShow output =\n%s\nwant\n%s", printed[0], want)
	}
}
