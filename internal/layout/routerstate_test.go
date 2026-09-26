package layout

// White-box (package layout, not layout_test) purely by convention with the
// rest of this package's tests; RouterStateDir needs no GOOS branching any
// more (#80), but staying in-package keeps this file next to the function it
// covers.

import (
	"path/filepath"
	"testing"
)

func TestRouterStateDir(t *testing.T) {
	getenv := func(values map[string]string) Getenv {
		return func(key string) (string, bool) {
			v, ok := values[key]
			return v, ok
		}
	}

	t.Run("lives under the berth root, alongside the control home", func(t *testing.T) {
		got, err := RouterStateDir(getenv(map[string]string{"HOME": "/home/dev"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join("/home/dev", ".berth", "router")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("follows BERTH_HOME, not BERTH_CONTROL_HOME", func(t *testing.T) {
		got, err := RouterStateDir(getenv(map[string]string{
			"HOME":               "/home/dev",
			"BERTH_HOME":         "/custom/home",
			"BERTH_CONTROL_HOME": "/elsewhere/control",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join("/custom/home", "router")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestRouterState(t *testing.T) {
	t.Run("derives every path under the state directory", func(t *testing.T) {
		getenv := func(key string) (string, bool) {
			if key == "HOME" {
				return "/home/dev", true
			}
			return "", false
		}
		state, err := RouterState(getenv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		dir, err := RouterStateDir(getenv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.StateDir != dir {
			t.Fatalf("StateDir = %q, want %q", state.StateDir, dir)
		}
		want := RouterLayout{
			StateDir:     dir,
			ComposeFile:  filepath.Join(dir, "compose.yaml"),
			ProtocolFile: filepath.Join(dir, "protocol"),
			DynamicDir:   filepath.Join(dir, "dynamic"),
			IndexDir:     filepath.Join(dir, "index"),
			IndexFile:    filepath.Join(dir, "index", "index.html"),
			LockPath:     filepath.Join(dir, "lock"),
		}
		if state != want {
			t.Fatalf("got %+v, want %+v", state, want)
		}
	})
}
