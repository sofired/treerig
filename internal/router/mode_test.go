package router_test

import (
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/router"
)

// nativeLinux is the engine this project's Linux machine reports: values
// captured from `docker info --format '{{json .}}'` on Ubuntu 24.04 with a
// rootful Docker Engine 29.
func nativeLinux() router.Engine {
	return router.Engine{
		OperatingSystem: "Ubuntu 24.04.4 LTS", Name: "cc-worker", OSType: "linux",
		SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"},
		Endpoint:        "unix:///var/run/docker.sock", ClientOS: "linux",
	}
}

// with is e after change has edited a copy of it.
func with(e router.Engine, change func(*router.Engine)) router.Engine {
	change(&e)
	return e
}

func TestDetectMode(t *testing.T) {
	desktop := func(e *router.Engine) { e.OperatingSystem, e.Name = "Docker Desktop", "docker-desktop" }
	cases := []struct {
		name     string
		engine   router.Engine
		override string
		want     router.Mode
		// refused is a phrase the refusal must contain; "" means accepted.
		refused string
	}{
		{name: "a native rootful Linux engine under a Linux client is host", engine: nativeLinux(), want: router.ModeHost},
		{
			name: "Docker Desktop on macOS is bridge",
			engine: with(nativeLinux(), func(e *router.Engine) {
				desktop(e)
				e.ClientOS = "darwin"
				e.Endpoint = "unix:///Users/dev/.docker/run/docker.sock"
			}),
			want: router.ModeBridge,
		},
		{name: "Docker Desktop on Linux is bridge: its engine is a VM", engine: with(nativeLinux(), desktop), want: router.ModeBridge},
		{
			name: "OrbStack is bridge",
			engine: with(nativeLinux(), func(e *router.Engine) {
				e.OperatingSystem, e.Name, e.ClientOS = "OrbStack", "orbstack", "darwin"
			}),
			want: router.ModeBridge,
		},
		{
			name:   "an engine named only by its host name is still recognized",
			engine: with(nativeLinux(), func(e *router.Engine) { e.OperatingSystem, e.Name = "Alpine Linux", "orbstack" }),
			want:   router.ModeBridge,
		},
		{
			name:    "a rootless daemon is refused and points at #121",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.SecurityOptions = []string{"name=seccomp,profile=builtin", "name=rootless"} }),
			refused: "#121",
		},
		{
			name:    "an ssh endpoint is remote and refused",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "ssh://dev@build-host" }),
			refused: "ssh://dev@build-host",
		},
		{
			name:    "TCP to another machine is remote and refused",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "tcp://10.0.0.5:2375" }),
			refused: "tcp://10.0.0.5:2375",
		},
		{
			name:   "TCP to this machine's own loopback is local",
			engine: with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "tcp://127.0.0.1:2375" }),
			want:   router.ModeHost,
		},
		{
			name:   "TCP to localhost is local",
			engine: with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "tcp://localhost:2375" }),
			want:   router.ModeHost,
		},
		{
			name:    "an endpoint that is not a URL is refused",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "::bad::" }),
			refused: "::bad::",
		},
		{
			name:    "a Windows-container engine is refused",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.OSType = "windows" }),
			refused: `OSType "windows"`,
		},
		{
			name: "a Linux engine reached from a macOS client (a VM such as Colima) is unrecognized and names the settings file",
			engine: with(nativeLinux(), func(e *router.Engine) {
				e.OperatingSystem, e.Name, e.ClientOS = "Ubuntu 24.04 LTS", "colima", "darwin"
				e.Endpoint = "unix:///Users/dev/.colima/default/docker.sock"
			}),
			refused: "/treerig/settings.toml",
		},
		{
			name: "the override host accepts that VM engine",
			engine: with(nativeLinux(), func(e *router.Engine) {
				e.OperatingSystem, e.Name, e.ClientOS = "Ubuntu 24.04 LTS", "colima", "darwin"
			}),
			override: "host", want: router.ModeHost,
		},
		{
			name: "the override bridge accepts that VM engine",
			engine: with(nativeLinux(), func(e *router.Engine) {
				e.OperatingSystem, e.Name, e.ClientOS = "Ubuntu 24.04 LTS", "colima", "darwin"
			}),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a Windows-container engine is told to switch to Linux containers",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.OSType = "windows" }),
			refused: "Switch Docker to Linux containers.",
		},
		{
			name:    "a remote refusal says an ssh:// or tcp:// host is remote",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "tcp://10.0.0.5:2375" }),
			refused: "An ssh:// or tcp:// host is remote.",
		},
		{
			name:    "a VM engine named colima-foo is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima-foo", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:   "a native host named limassol is not a Lima engine",
			engine: with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "limassol", "linux" }),
			want:   router.ModeHost,
		},
		{
			name:   "a native host named limbo is not a Lima engine",
			engine: with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "limbo", "linux" }),
			want:   router.ModeHost,
		},
		{
			name:   "a native host named colimator is not a Colima engine",
			engine: with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colimator", "linux" }),
			want:   router.ModeHost,
		},
		{
			name:    "a VM engine named colima under a linux client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine colima under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "linux" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine colima under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "linux" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named colima under a darwin client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "darwin" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine colima under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "darwin" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine colima under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", "darwin" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named lima-default under a linux client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine lima-default under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "linux" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine lima-default under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "linux" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named lima-default under a darwin client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "darwin" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine lima-default under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "darwin" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine lima-default under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-default", "darwin" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named rancher-desktop under a linux client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine rancher-desktop under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "linux" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine rancher-desktop under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "linux" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named rancher-desktop under a darwin client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "darwin" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine rancher-desktop under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "darwin" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine rancher-desktop under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "rancher-desktop", "darwin" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named lima-rancher-desktop under a linux client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine lima-rancher-desktop under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "linux" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine lima-rancher-desktop under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "linux" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named lima-rancher-desktop under a darwin client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "darwin" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine lima-rancher-desktop under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "darwin" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine lima-rancher-desktop under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "lima-rancher-desktop", "darwin" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named Colima under a linux client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "linux" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine Colima under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "linux" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine Colima under a linux client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "linux" }),
			override: "bridge", want: router.ModeBridge,
		},
		{
			name:    "a VM engine named Colima under a darwin client is refused unless the mode is set",
			engine:  with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "darwin" }),
			refused: "Set [router] mode in /treerig/settings.toml",
		},
		{
			name:     "the override host accepts the VM engine Colima under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "darwin" }),
			override: "host", want: router.ModeHost,
		},
		{
			name:     "the override bridge accepts the VM engine Colima under a darwin client",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "Colima", "darwin" }),
			override: "bridge", want: router.ModeBridge,
		},
		{name: "the override replaces what a recognized engine would choose", engine: nativeLinux(), override: "bridge", want: router.ModeBridge},
		{name: "an unknown override value changes nothing", engine: nativeLinux(), override: "auto", want: router.ModeHost},
		{
			name:     "the override never bypasses a rootless daemon",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.SecurityOptions = []string{"name=rootless"} }),
			override: "host", refused: "#121",
		},
		{
			name:     "the override never bypasses a remote daemon",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.Endpoint = "tcp://10.0.0.5:2375" }),
			override: "bridge", refused: "tcp://10.0.0.5:2375",
		},
		{
			name:     "the override never bypasses a Windows-container engine",
			engine:   with(nativeLinux(), func(e *router.Engine) { e.OSType = "windows" }),
			override: "host", refused: "windows",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := router.DetectMode(tc.engine, tc.override, "/treerig/settings.toml")
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("DetectMode = %q, %v; want a refusal containing %q", got, err, tc.refused)
				}
				if got != "" {
					t.Errorf("a refusal also returned the mode %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DetectMode = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestParseEngineInfo(t *testing.T) {
	t.Run("reads the fields detection needs out of `docker info --format '{{json .}}'`", func(t *testing.T) {
		// A trimmed copy of a real answer; the fields detection ignores are present on purpose.
		document := `{"ID":"7043a131","Containers":4,"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux",` +
			`"Architecture":"x86_64","ServerVersion":"29.7.2","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"],` +
			`"Labels":[],"ClientInfo":{"Context":"default"}}`
		engine, err := router.ParseEngineInfo(document)
		if err != nil {
			t.Fatal(err)
		}
		want := router.Engine{
			OperatingSystem: "Ubuntu 24.04.4 LTS", Name: "cc-worker", OSType: "linux",
			SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"},
		}
		if engine.OperatingSystem != want.OperatingSystem || engine.Name != want.Name || engine.OSType != want.OSType ||
			strings.Join(engine.SecurityOptions, ",") != strings.Join(want.SecurityOptions, ",") {
			t.Fatalf("engine = %+v, want %+v", engine, want)
		}
		if engine.Endpoint != "" || engine.ClientOS != "" {
			t.Errorf("the endpoint and client OS are the caller's to fill in: %+v", engine)
		}
	})

	t.Run("reports an answer that is not JSON", func(t *testing.T) {
		if _, err := router.ParseEngineInfo("Client:\n Version: 29"); err == nil || !strings.Contains(err.Error(), "Could not read `docker info`") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDetectModeRefusalWording(t *testing.T) {
	vm := func(client string) router.Engine {
		return with(nativeLinux(), func(e *router.Engine) { e.Name, e.ClientOS = "colima", client })
	}
	message := func(t *testing.T, engine router.Engine) string {
		t.Helper()
		_, err := router.DetectMode(engine, "", "/treerig/settings.toml")
		if err == nil {
			t.Fatal("expected a refusal")
		}
		return err.Error()
	}

	t.Run("leads with bridge for a non-Linux client", func(t *testing.T) {
		got := message(t, vm("darwin"))
		if bridge, host := strings.Index(got, `"bridge"`), strings.Index(got, `"host"`); bridge < 0 || host < 0 || bridge > host {
			t.Errorf("bridge should come first: %s", got)
		}
	})

	t.Run("leads with host for a Linux client", func(t *testing.T) {
		got := message(t, vm("linux"))
		if bridge, host := strings.Index(got, `"bridge"`), strings.Index(got, `"host"`); bridge < 0 || host < 0 || host > bridge {
			t.Errorf("host should come first: %s", got)
		}
	})

	t.Run("says the VM path is untested", func(t *testing.T) {
		if got := message(t, vm("darwin")); !strings.Contains(got, "untested") {
			t.Errorf("no untested caveat: %s", got)
		}
	})
}
