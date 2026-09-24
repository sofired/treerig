package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/clierr"
)

// Ported from tests/unit/config.test.ts, one t.Run per "it" title, nested
// under its "describe" the way vitest groups them, so coverage can be
// compared title by title. See corpus_test.go for the larger byte-for-byte
// oracle comparison against real parseConfig; these exercise the same
// package through its public API the way the original suite does.

const minimalTOML = `
[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`

func TestConfigParsing(t *testing.T) {
	t.Run("accepts a minimal file and fills defaults", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Project.Name != "acme" {
			t.Errorf("Project.Name = %q, want %q", cfg.Project.Name, "acme")
		}
		if len(cfg.Service) != 0 {
			t.Errorf("Service = %#v, want empty", cfg.Service)
		}
		if cfg.Hooks != (Hooks{}) {
			t.Errorf("Hooks = %#v, want the zero value", cfg.Hooks)
		}
	})

	t.Run("defaults a service to the host root and keeps index metadata", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+`
[[service]]
name = "api"
[service.index]
label = "API"
`), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(cfg.Service) != 1 {
			t.Fatalf("Service has %d entries, want 1", len(cfg.Service))
		}
		got := cfg.Service[0]
		if got.Name != "api" || got.Subdomain != "" || got.Path != nil || got.Port != nil {
			t.Errorf("Service[0] = %#v, want name=api subdomain=\"\" path=nil port=nil", got)
		}
		if got.Index == nil || got.Index.Label != "API" || got.Index.Purpose != "" {
			t.Errorf("Service[0].Index = %#v, want {Label:API Purpose:\"\"}", got.Index)
		}
	})

	t.Run("rejects two services claiming the same subdomain", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+`
[[service]]
name = "api"
[[service]]
name = "web"
`), "test.toml")
		requireErrorContains(t, err, "the host root is already claimed")
	})

	t.Run("lets two services share a host when their paths differ, and rejects the same path twice", func(t *testing.T) {
		shared := minimalTOML + `
[[service]]
name = "web"
[[service]]
name = "api"
path = "/api"
port = 3000
`
		cfg, err := Parse([]byte(shared), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(cfg.Service) != 2 {
			t.Fatalf("Service has %d entries, want 2", len(cfg.Service))
		}
		if cfg.Service[0].Name != "web" || cfg.Service[0].Path != nil || cfg.Service[0].Port != nil {
			t.Errorf("Service[0] = %#v", cfg.Service[0])
		}
		if cfg.Service[1].Name != "api" || cfg.Service[1].Path == nil || *cfg.Service[1].Path != "/api" ||
			cfg.Service[1].Port == nil || *cfg.Service[1].Port != 3000 {
			t.Errorf("Service[1] = %#v", cfg.Service[1])
		}

		_, err = Parse([]byte(shared+"[[service]]\nname = \"api2\"\npath = \"/api\"\n"), "test.toml")
		requireErrorContains(t, err, `the host root at path "/api" is already claimed`)

		_, err = Parse([]byte(minimalTOML+"[[service]]\nname = \"a\"\nport = 70000\n"), "test.toml")
		requireErrorContains(t, err, "port")
	})

	t.Run("keeps what reaches Traefik rules and hostnames well-formed", func(t *testing.T) {
		service := func(body string) string {
			return minimalTOML + "[[service]]\nname = \"s\"\n" + body
		}
		_, err := Parse([]byte(service("path = \"/api`)||Host(`evil\"\n")), "test.toml")
		requireErrorContains(t, err, "must be a URL path starting with /")

		_, err = Parse([]byte(service("path = \"/a%zz\"\n")), "test.toml")
		requireErrorContains(t, err, "% must be followed by two hex digits")

		_, err = Parse([]byte(service("path = \"/search?q=1\"\n")), "test.toml")
		requireErrorContains(t, err, "must be a URL path starting with /")

		// Compose would interpolate it.
		_, err = Parse([]byte(service("path = \"/api$HOME\"\n")), "test.toml")
		requireErrorContains(t, err, "backticks, $, ? or #")

		cfg, err := Parse([]byte(service("path = \"/v1/things:list/@scope/c++%2Fx\"\n")), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got, want := *cfg.Service[0].Path, "/v1/things:list/@scope/c++%2Fx"; got != want {
			t.Errorf("Path = %q, want %q", got, want)
		}

		_, err = Parse([]byte(service("subdomain = \"-mail\"\n")), "test.toml")
		requireErrorContains(t, err, "not starting or ending with a hyphen")

		_, err = Parse([]byte(service("subdomain = \"mail-\"\n")), "test.toml")
		requireErrorContains(t, err, "not starting or ending with a hyphen")

		_, err = Parse([]byte(service("subdomain = \""+strings.Repeat("a", 64)+"\"\n")), "test.toml")
		requireErrorContains(t, err, "at most 63")

		longLabel := strings.ReplaceAll(minimalTOML, "acme.localhost", strings.Repeat("a", 64)+".localhost")
		_, err = Parse([]byte(longLabel), "test.toml")
		requireErrorContains(t, err, "each label must be at most 63")

		longDomain := strings.ReplaceAll(minimalTOML, "acme.localhost", strings.Repeat("ab.", 40)+"localhost")
		_, err = Parse([]byte(longDomain), "test.toml")
		requireErrorContains(t, err, "at most 125 characters, so that <subdomain>.<slug>.<domain> fits")
	})

	t.Run("rejects duplicate service names", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+`
[[service]]
name = "api"
subdomain = "a"
[[service]]
name = "api"
subdomain = "b"
`), "test.toml")
		requireErrorContains(t, err, `duplicate service name "api"`)
	})

	// (#16, row 16 in the divergence ledger) Since `up` now records every
	// declared service on the checkout index card, not only the ones with a
	// [service.index] block, the config's own [[service]] array must not
	// outgrow what a card can hold (internal/checkoutindex/card's own
	// maxServices). TypeScript has no such limit — this is a deliberate,
	// Go-only bound.
	t.Run("rejects more than 64 declared services", func(t *testing.T) {
		var services strings.Builder
		for i := 0; i < 65; i++ {
			fmt.Fprintf(&services, "[[service]]\nname = \"s%d\"\nsubdomain = \"s%d\"\n", i, i)
		}
		_, err := Parse([]byte(minimalTOML+services.String()), "test.toml")
		requireErrorContains(t, err, "at most 64 [[service]] entries")
	})

	t.Run("accepts exactly 64 declared services", func(t *testing.T) {
		var services strings.Builder
		for i := 0; i < 64; i++ {
			fmt.Fprintf(&services, "[[service]]\nname = \"s%d\"\nsubdomain = \"s%d\"\n", i, i)
		}
		cfg, err := Parse([]byte(minimalTOML+services.String()), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(cfg.Service) != 64 {
			t.Fatalf("Service has %d entries, want 64", len(cfg.Service))
		}
	})

	t.Run("rejects unknown keys so typos do not silently disable features", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"\n[hooks]\nprepair = \"./x\"\n"), "test.toml")
		requireErrorContains(t, err, "hooks")
	})

	t.Run("reports invalid TOML distinctly from invalid configuration", func(t *testing.T) {
		_, err := Parse([]byte("[project\nname = "), "test.toml")
		requireErrorContains(t, err, "is not valid TOML")
	})

	t.Run("refuses a control home in the committed config, since that path is per-machine", func(t *testing.T) {
		toml := strings.Replace(minimalTOML, "domain", "control-home = \"~/acme\"\ndomain", 1)
		_, err := Parse([]byte(toml), "test.toml")
		requireErrorContains(t, err, "control-home")
	})

	t.Run("validates the label namespace shape", func(t *testing.T) {
		toml := strings.Replace(minimalTOML, "domain", "label-namespace = \"NotADomain\"\ndomain", 1)
		_, err := Parse([]byte(toml), "test.toml")
		requireErrorContains(t, err, "reverse domain")
	})
}

func TestConfigDiscovery(t *testing.T) {
	t.Run("walks upward from a nested directory to the nearest config file", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "apps", "api", "src")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, "berth.toml")
		if err := os.WriteFile(file, []byte(minimalTOML), 0o644); err != nil {
			t.Fatal(err)
		}

		found, ok, err := FindConfigFile(nested)
		if err != nil || !ok || found != file {
			t.Fatalf("FindConfigFile = (%q, %v, %v), want (%q, true, nil)", found, ok, err, file)
		}

		loaded, err := LoadNearest(nested)
		if err != nil {
			t.Fatalf("LoadNearest: %v", err)
		}
		if loaded.File != file {
			t.Errorf("loaded.File = %q, want %q", loaded.File, file)
		}
		if loaded.Root != root {
			t.Errorf("loaded.Root = %q, want %q", loaded.Root, root)
		}
	})

	t.Run("fails with a hint when no config file exists", func(t *testing.T) {
		root := t.TempDir()
		_, err := LoadNearest(root)
		if err == nil {
			t.Fatal("LoadNearest: expected an error, got none")
		}
		if !clierr.Expected(err) {
			t.Errorf("LoadNearest error is not a CliError-equivalent: %v", err)
		}
		requireErrorContains(t, err, "No berth.toml found")
	})
}

func requireErrorContains(t *testing.T, err error, substring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got none", substring)
	}
	if !strings.Contains(err.Error(), substring) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), substring)
	}
}
