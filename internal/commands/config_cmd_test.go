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
	if want := "control home: " + filepath.Join(home, ".berth"); printed[3] != want {
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
