package config

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sharedFile is a minimal config with a [shared] table; the cases append
// [shared.service.<name>] tables to it.
const sharedFile = minimalTOML + "[service.web]\n[service.worker]\nsubdomain = \"w\"\n[shared]\nfile = \"compose.shared.yaml\"\n"

func TestSharedConfig(t *testing.T) {
	t.Run("reads shared services in the file's order, with every port and consumer", func(t *testing.T) {
		cfg, err := Parse([]byte(sharedFile+`
[shared.service.mailpit]
port = 8025
ports = [1025, 1110]
used-by = ["web", "worker"]
[shared.service.db]
ports = [5432]
used-by = ["web"]
`), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Shared == nil || cfg.Shared.File != "compose.shared.yaml" {
			t.Fatalf("Shared = %+v", cfg.Shared)
		}
		services := cfg.SharedServices()
		if len(services) != 2 || services[0].Name != "mailpit" || services[1].Name != "db" {
			t.Fatalf("services = %+v, want mailpit then db", services)
		}
		mailpit := services[0]
		if mailpit.Port == nil || *mailpit.Port != 8025 || !slices.Equal(mailpit.Ports, []int{1025, 1110}) || !slices.Equal(mailpit.UsedBy, []string{"web", "worker"}) {
			t.Errorf("mailpit = %+v", mailpit)
		}
		if got := mailpit.ContainerPorts(); !slices.Equal(got, []int{8025, 1025, 1110}) {
			t.Errorf("ContainerPorts = %v, want the web UI first, then the raw ports", got)
		}
		if db := services[1]; db.Port != nil || !slices.Equal(db.ContainerPorts(), []int{5432}) {
			t.Errorf("db = %+v", db)
		}
	})

	t.Run("a service with nothing but its table has no ports and no consumers, never nil", func(t *testing.T) {
		cfg, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\n"), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got := cfg.SharedServices()[0]
		if got.Port != nil || got.Ports == nil || got.UsedBy == nil || len(got.ContainerPorts()) != 0 {
			t.Errorf("service = %+v", got)
		}
	})

	t.Run("a file without [shared] has none, and config show leaves the key out", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Shared != nil || cfg.SharedServices() != nil {
			t.Errorf("Shared = %+v", cfg.Shared)
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"shared"`) {
			t.Errorf("config show prints %s", encoded)
		}
		withShared, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\nused-by = [\"web\"]\n"), "test.toml")
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ = json.Marshal(withShared)
		if !strings.Contains(string(encoded), `"shared":{"file":"compose.shared.yaml","service":[{"name":"mailpit","ports":[],"used-by":["web"]}]}`) {
			t.Errorf("config show prints %s", encoded)
		}
	})

	t.Run("rejects [[shared.service]], saying shared services are tables keyed by name", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[[shared.service]]\nname = \"mailpit\"\n"), "test.toml")
		requireErrorContains(t, err, "shared.service: Invalid input: expected object, received array (shared services are tables keyed by name: write [shared.service.<name>], not [[shared.service]])")
	})

	t.Run("says the table's key is the name when a name is written inside", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\nname = \"mailpit\"\n"), "test.toml")
		requireErrorContains(t, err, "the table's key is the service name: [shared.service.mailpit]")
	})

	t.Run("needs a file as soon as one service is declared", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[shared.service.mailpit]\n"), "test.toml")
		requireErrorContains(t, err, "shared.file: required when any [shared.service.<name>] table is declared")
	})

	t.Run("does not need a file for an empty [shared]", func(t *testing.T) {
		if _, err := Parse([]byte(minimalTOML+"[shared]\n"), "test.toml"); err != nil {
			t.Fatalf("Parse: %v", err)
		}
	})

	t.Run("refuses the shared file being the worktree's own Compose file", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[shared]\nfile = \"./docker/compose.yaml\"\n[shared.service.mailpit]\n"), "test.toml")
		requireErrorContains(t, err, "shared.file: must not be the [compose] file")
	})

	t.Run("refuses the shared file being the Compose file however each is written", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[shared]\nfile = \"/etc/berth/docker/compose.yaml\"\n[shared.service.mailpit]\n"), "/etc/berth/berth.toml")
		requireErrorContains(t, err, "shared.file: must not be the [compose] file")
		_, err = Parse([]byte(minimalTOML+"[shared]\nfile = \"docker/../docker/compose.yaml\"\n[shared.service.mailpit]\n"), "test.toml")
		requireErrorContains(t, err, "shared.file: must not be the [compose] file")
	})

	t.Run("refuses a shared name that is also a [service.<name>] table", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[shared.service.web]\n"), "test.toml")
		requireErrorContains(t, err, `shared.service.web: "web" is also a [service.web] table`)
	})

	t.Run("refuses a used-by listed twice, and one that names a shared service", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\nused-by = [\"web\", \"worker\", \"web\"]\n"), "test.toml")
		requireErrorContains(t, err, `shared.service.mailpit.used-by.2: service "web" is listed twice`)
		_, err = Parse([]byte(sharedFile+"[shared.service.mailpit]\n[shared.service.db]\nused-by = [\"mailpit\"]\n"), "test.toml")
		requireErrorContains(t, err, `shared.service.db.used-by.0: "mailpit" is a shared service`)
	})

	t.Run("refuses a port declared twice, and more than sixteen in all", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\nport = 1025\nports = [1025]\n"), "test.toml")
		requireErrorContains(t, err, "shared.service.mailpit.ports.0: port 1025 is listed more than once for this service")
		_, err = Parse([]byte(sharedFile+"[shared.service.mailpit]\nports = [1, 2, 1]\n"), "test.toml")
		requireErrorContains(t, err, "shared.service.mailpit.ports.2: port 1 is listed more than once for this service")

		many := make([]string, 16)
		for i := range many {
			many[i] = strconv.Itoa(1000 + i)
		}
		_, err = Parse([]byte(sharedFile+"[shared.service.mailpit]\nport = 9999\nports = ["+strings.Join(many, ", ")+"]\n"), "test.toml")
		requireErrorContains(t, err, "shared.service.mailpit.ports: at most 16 ports per shared service, port and ports together; this declares 17")
	})

	t.Run("refuses a port outside 1 to 65535", func(t *testing.T) {
		_, err := Parse([]byte(sharedFile+"[shared.service.mailpit]\nports = [70000]\n"), "test.toml")
		if err == nil {
			t.Fatal("accepted a port of 70000")
		}
		_, err = Parse([]byte(sharedFile+"[shared.service.mailpit]\nport = 0\n"), "test.toml")
		if err == nil {
			t.Fatal("accepted a port of 0")
		}
	})
}
