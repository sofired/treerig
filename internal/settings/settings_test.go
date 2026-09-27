package settings

// settings.toml is issue #79 (#46): these test titles are this package's own.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// mapGetenv adapts a map to layout.Getenv, so a test can fix an environment
// without touching the process's real one.
func mapGetenv(values map[string]string) layout.Getenv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

// requireErrorContains fails t unless err is non-nil and its message
// contains substr.
func requireErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want it to contain %q", err, substr)
	}
}

func TestParse(t *testing.T) {
	t.Run("accepts an empty file, applying every default", func(t *testing.T) {
		got, err := Parse([]byte(""), "settings.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got.Router.Port != nil {
			t.Errorf("Router.Port = %v, want nil", got.Router.Port)
		}
	})

	t.Run("accepts a valid [router] port", func(t *testing.T) {
		got, err := Parse([]byte("[router]\nport = 8080\n"), "settings.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got.Router.Port == nil || *got.Router.Port != 8080 {
			t.Errorf("Router.Port = %v, want 8080", got.Router.Port)
		}
	})

	t.Run("rejects an unknown top-level key, naming the file", func(t *testing.T) {
		_, err := Parse([]byte("color = \"blue\"\n"), "/x/settings.toml")
		requireErrorContains(t, err, "/x/settings.toml is not a valid settings.toml:")
		requireErrorContains(t, err, `Unrecognized key: "color"`)
	})

	t.Run("rejects an unknown key inside [router]", func(t *testing.T) {
		_, err := Parse([]byte("[router]\nport = 80\nhost = \"x\"\n"), "settings.toml")
		requireErrorContains(t, err, `router: Unrecognized key: "host"`)
	})

	t.Run("rejects a port of the wrong type", func(t *testing.T) {
		_, err := Parse([]byte("[router]\nport = \"80\"\n"), "settings.toml")
		requireErrorContains(t, err, "router.port: Invalid input: expected number, received string")
	})

	t.Run("rejects a port outside 1-65535", func(t *testing.T) {
		_, err := Parse([]byte("[router]\nport = 0\n"), "settings.toml")
		requireErrorContains(t, err, "router.port:")

		_, err = Parse([]byte("[router]\nport = 70000\n"), "settings.toml")
		requireErrorContains(t, err, "router.port:")
	})

	t.Run("reports a TOML syntax error naming the file", func(t *testing.T) {
		_, err := Parse([]byte("[router\n"), "/x/settings.toml")
		requireErrorContains(t, err, "/x/settings.toml is not valid TOML:")
	})
}

func TestLoad(t *testing.T) {
	t.Run("resolves a missing file to the zero value, not an error", func(t *testing.T) {
		got, err := Load(filepath.Join(registrytest.TempDir(t), "settings.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Router.Port != nil {
			t.Errorf("Router.Port = %v, want nil", got.Router.Port)
		}
	})

	t.Run("loads and validates a file that exists", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "settings.toml")
		if err := writeFile(path, "[router]\nport = 9090\n"); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Router.Port == nil || *got.Router.Port != 9090 {
			t.Errorf("Router.Port = %v, want 9090", got.Router.Port)
		}
	})

	t.Run("names the file when it exists but cannot be read", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "settings.toml")
		if err := os.Mkdir(path, 0o700); err != nil { // a directory where the file belongs: readable as neither
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.HasPrefix(err.Error(), "Could not read "+path+": ") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("names the file and the key in a malformed file's error", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		path := filepath.Join(dir, "settings.toml")
		if err := writeFile(path, "[router]\nport = -1\n"); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		requireErrorContains(t, err, path+" is not a valid settings.toml:")
		requireErrorContains(t, err, "router.port:")
	})
}

func TestResolveRouterPort(t *testing.T) {
	t.Run("defaults to 80 when neither the environment nor the file set a port", func(t *testing.T) {
		home := registrytest.TempDir(t)
		got, err := ResolveRouterPort(mapGetenv(nil), home)
		if err != nil {
			t.Fatalf("ResolveRouterPort: %v", err)
		}
		want := PortResolution{Port: 80, Source: PortDefault, SettingsPath: layout.SettingsPathFrom(home)}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("prefers the settings file over the default", func(t *testing.T) {
		home := registrytest.TempDir(t)
		if err := writeFile(layout.SettingsPathFrom(home), "[router]\nport = 8080\n"); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveRouterPort(mapGetenv(nil), home)
		if err != nil {
			t.Fatalf("ResolveRouterPort: %v", err)
		}
		if got.Port != 8080 || got.Source != PortFile {
			t.Errorf("got %+v, want port 8080 from the file", got)
		}
	})

	t.Run("prefers BERTH_ROUTER_PORT over the settings file", func(t *testing.T) {
		home := registrytest.TempDir(t)
		if err := writeFile(layout.SettingsPathFrom(home), "[router]\nport = 8080\n"); err != nil {
			t.Fatal(err)
		}
		env := mapGetenv(map[string]string{layout.RouterPortEnv: "9090"})
		got, err := ResolveRouterPort(env, home)
		if err != nil {
			t.Fatalf("ResolveRouterPort: %v", err)
		}
		if got.Port != 9090 || got.Source != PortEnv {
			t.Errorf("got %+v, want port 9090 from the environment", got)
		}
	})

	t.Run("reports BERTH_ROUTER_PORT's own error, never reaching the file", func(t *testing.T) {
		home := registrytest.TempDir(t)
		if err := writeFile(layout.SettingsPathFrom(home), "[router]\nport = 8080\n"); err != nil {
			t.Fatal(err)
		}
		env := mapGetenv(map[string]string{layout.RouterPortEnv: "not-a-port"})
		_, err := ResolveRouterPort(env, home)
		requireErrorContains(t, err, `BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "not-a-port".`)
	})

	t.Run("reports the settings file's own error when it is malformed", func(t *testing.T) {
		home := registrytest.TempDir(t)
		if err := writeFile(layout.SettingsPathFrom(home), "[router]\nport = \"nope\"\n"); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveRouterPort(mapGetenv(nil), home)
		requireErrorContains(t, err, "is not a valid settings.toml:")
	})
}

func TestPortSourceString(t *testing.T) {
	t.Run("names each source", func(t *testing.T) {
		cases := []struct {
			source PortSource
			want   string
		}{
			{PortDefault, "the default"},
			{PortFile, "the settings file"},
			{PortEnv, "BERTH_ROUTER_PORT"},
		}
		for _, c := range cases {
			if got := c.source.String(); got != c.want {
				t.Errorf("%v.String() = %q, want %q", c.source, got, c.want)
			}
		}
	})
}

// writeFile writes contents to path, creating its parent directory first.
func writeFile(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0o600)
}

func TestPortResolutionDescribe(t *testing.T) {
	t.Run("names the port, its source and the settings file", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   PortResolution
			want string
		}{
			{"from the environment", PortResolution{Port: 8080, Source: PortEnv, SettingsPath: "/b/settings.toml"}, "port 8080 from BERTH_ROUTER_PORT (overriding /b/settings.toml)"},
			{"from the file", PortResolution{Port: 8080, Source: PortFile, SettingsPath: "/b/settings.toml"}, "port 8080 from /b/settings.toml"},
			{"the default", PortResolution{Port: 80, Source: PortDefault, SettingsPath: "/b/settings.toml"}, "port 80 by default (/b/settings.toml sets none)"},
		} {
			if got := tc.in.Describe(); got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		}
	})
}
