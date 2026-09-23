package layout

// White-box (package layout, not layout_test) so both GOOS branches of
// routerStateDirFor can be exercised from either platform, the same reason
// lock.Options exposes Clock rather than calling time.Now directly.

import (
	"path/filepath"
	"testing"
)

func TestRouterStateDirFor(t *testing.T) {
	getenv := func(values map[string]string) Getenv {
		return func(key string) (string, bool) {
			v, ok := values[key]
			return v, ok
		}
	}

	t.Run("uses Library/Application Support on macOS, ignoring XDG_STATE_HOME", func(t *testing.T) {
		got, err := routerStateDirFor("darwin", getenv(map[string]string{
			"HOME":           "/Users/dev",
			"XDG_STATE_HOME": "/should/not/matter",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join("/Users/dev", "Library", "Application Support", "berth", "router")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("prefers XDG_STATE_HOME on Linux", func(t *testing.T) {
		got, err := routerStateDirFor("linux", getenv(map[string]string{
			"HOME":           "/home/dev",
			"XDG_STATE_HOME": "/custom/state",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join("/custom/state", "berth", "router")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to ~/.local/state on Linux when XDG_STATE_HOME is unset or empty", func(t *testing.T) {
		for _, env := range []map[string]string{
			{"HOME": "/home/dev"},
			{"HOME": "/home/dev", "XDG_STATE_HOME": ""},
		} {
			got, err := routerStateDirFor("linux", getenv(env))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := filepath.Join("/home/dev", ".local", "state", "berth", "router")
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		}
	})

	t.Run("takes the Linux path on any other GOOS, since there is no Windows build", func(t *testing.T) {
		got, err := routerStateDirFor("freebsd", getenv(map[string]string{"HOME": "/home/dev"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join("/home/dev", ".local", "state", "berth", "router")
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
