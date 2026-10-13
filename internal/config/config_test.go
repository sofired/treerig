package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/clierr"
)

// See corpus_test.go for the larger byte-for-byte fixture comparison
// against real parseConfig; these exercise the same package through its
// public API.

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
[service.api]
[service.api.index]
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
[service.api]
[service.web]
`), "test.toml")
		requireErrorContains(t, err, "the host root is already claimed")
	})

	t.Run("lets two services share a host when their paths differ, and rejects the same path twice", func(t *testing.T) {
		shared := minimalTOML + `
[service.web]
[service.api]
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

		_, err = Parse([]byte(shared+"[service.api2]\npath = \"/api\"\n"), "test.toml")
		requireErrorContains(t, err, `the host root at path "/api" is already claimed`)

		_, err = Parse([]byte(minimalTOML+"[service.a]\nport = 70000\n"), "test.toml")
		requireErrorContains(t, err, "port")
	})

	t.Run("keeps what reaches Traefik rules and hostnames well-formed", func(t *testing.T) {
		service := func(body string) string {
			return minimalTOML + "[service.s]\n" + body
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

	t.Run("rejects a service table defined twice, as TOML itself does", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+`
[service.api]
subdomain = "a"
[service.api]
subdomain = "b"
`), "test.toml")
		requireErrorContains(t, err, "test.toml is not valid TOML:")
	})

	t.Run("rejects [[service]], saying services are tables keyed by name", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+`
[[service]]
name = "api"
`), "test.toml")
		requireErrorContains(t, err, "service: Invalid input: expected object, received array (services are tables keyed by name: write [service.<name>], not [[service]])")
	})

	t.Run("rejects a service name that is not a slug, at its key", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[service.Api]\n"), "test.toml")
		requireErrorContains(t, err, "service.Api: must be lower-case letters, digits, and hyphens")
	})

	t.Run("rejects a name field, which is now the table's key", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[service.api]\nname = \"api\"\n"), "test.toml")
		requireErrorContains(t, err, `service.api: Unrecognized key: "name" (the table's key is the service name: [service.api])`)
	})

	t.Run("names the service that already claimed a route", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[service.api]\nsubdomain = \"a\"\n[service.web]\nsubdomain = \"a\"\n"), "test.toml")
		requireErrorContains(t, err, `service.web.subdomain: subdomain "a" is already claimed by service "api"`)
	})

	t.Run("caps a service name at 63 characters, as a command name is", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[service."+strings.Repeat("a", 64)+"]\n"), "test.toml")
		requireErrorContains(t, err, "must be at most 63 characters")
		_, err = Parse([]byte(minimalTOML+"[service."+strings.Repeat("a", 63)+"]\n"), "test.toml")
		if err != nil {
			t.Errorf("63 characters: %v", err)
		}
	})

	t.Run("gives [[commands]] the same hint as [[service]]", func(t *testing.T) {
		_, err := Parse([]byte(minimalTOML+"[[commands]]\ncommand = \"x\"\n"), "test.toml")
		requireErrorContains(t, err, "commands are tables keyed by name: write [commands.<name>], not [[commands]]")
	})

	t.Run("keeps services in the order the file wrote them", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+"[service.web]\nsubdomain = \"w\"\n[service.api]\nsubdomain = \"a\"\n[service.mail]\nsubdomain = \"m\"\n"), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		var names []string
		for _, s := range cfg.Service {
			names = append(names, s.Name)
		}
		if got := strings.Join(names, ","); got != "web,api,mail" {
			t.Errorf("service order = %s, want web,api,mail", got)
		}
	})

	// (#16) Since `up` now records every declared service on the checkout
	// index card, not only the ones with a [service.<name>.index] table, the
	// config's own [service] table must not outgrow what a card can hold
	// (internal/checkoutindex/card's own maxServices) — a deliberate bound
	// of its own.
	t.Run("rejects more than 64 declared services", func(t *testing.T) {
		var services strings.Builder
		for i := 0; i < 65; i++ {
			fmt.Fprintf(&services, "[service.s%d]\nsubdomain = \"s%d\"\n", i, i)
		}
		_, err := Parse([]byte(minimalTOML+services.String()), "test.toml")
		requireErrorContains(t, err, "at most 64 [service.<name>] tables")
	})

	t.Run("accepts exactly 64 declared services", func(t *testing.T) {
		var services strings.Builder
		for i := 0; i < 64; i++ {
			fmt.Fprintf(&services, "[service.s%d]\nsubdomain = \"s%d\"\n", i, i)
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

	t.Run("accepts a label and purpose with ordinary punctuation and non-ASCII letters", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"Café — API\"\npurpose = \"the café's API\"\n"), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got, want := cfg.Service[0].Index.Label, "Café — API"; got != want {
			t.Errorf("Label = %q, want %q", got, want)
		}
	})

	// (#19) The base loader accepts a control character in label or
	// purpose, even though both are only ever shown through text.Printable,
	// which silently turns it into a space or U+FFFD — see TestStartIssues
	// below for the separate check `up`, `config validate` and `doctor` run
	// instead, so a command that only reads an already-running worktree's
	// config (`down`, `worktree remove`, `status`, and so on) can still
	// load this file and proceed.
	t.Run("accepts a control character in the service index label and purpose; only a command about to start something refuses one", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"a\tb\"\npurpose = \"the \\u202eAPI\"\n"), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got, want := cfg.Service[0].Index.Label, "a\tb"; got != want {
			t.Errorf("Label = %q, want %q", got, want)
		}
		if got, want := cfg.Service[0].Index.Purpose, "the \u202eAPI"; got != want {
			t.Errorf("Purpose = %q, want %q", got, want)
		}
	})
}

// TestStartIssues covers StartIssues and CheckStart: the check `up`,
// `config validate` and `doctor`'s "This checkout" section run separately
// from the base loader above (#19). The base loader stays lenient so every
// other command can load the same file and proceed; this is what refuses
// it instead, with the same message the
// old schema-level check printed.
func TestStartIssues(t *testing.T) {
	parse := func(t *testing.T, toml string) Config {
		t.Helper()
		cfg, err := Parse([]byte(toml), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return cfg
	}

	t.Run("finds nothing wrong with an ordinary label and purpose", func(t *testing.T) {
		cfg := parse(t, minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"Café — API\"\npurpose = \"the café's API\"\n")
		if issues := StartIssues(cfg); len(issues) != 0 {
			t.Fatalf("issues = %+v, want none", issues)
		}
	})

	t.Run("leaves a service with no [service.api.index] block alone", func(t *testing.T) {
		cfg := parse(t, minimalTOML+"[service.api]\n")
		if issues := StartIssues(cfg); len(issues) != 0 {
			t.Fatalf("issues = %+v, want none", issues)
		}
	})

	t.Run("reports a control character in the service index label", func(t *testing.T) {
		// TOML itself refuses a literal C0 control byte in a string (only
		// tab is allowed unescaped), so this reaches the check through
		// TOML's own \u escape instead — a case the TOML syntax error path
		// never reaches.
		cfg := parse(t, minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"a\\u001bb\"\n")
		err := CheckStart(&Loaded{Config: cfg, File: "test.toml"})
		requireErrorContains(t, err, "service.api.index.label: must not contain control characters")
	})

	t.Run("reports a control character in the service index purpose", func(t *testing.T) {
		// A right-to-left override, unlike a C0 control, is not something
		// TOML's own grammar refuses — it reaches the check as a plain
		// character in the file, the same way it would from a copy-paste.
		cfg := parse(t, minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"API\"\npurpose = \"the \\u202eAPI\"\n")
		err := CheckStart(&Loaded{Config: cfg, File: "test.toml"})
		requireErrorContains(t, err, "service.api.index.purpose: must not contain control characters")
	})

	t.Run("reports a tab in the service index label, even though TOML itself allows one unescaped", func(t *testing.T) {
		cfg := parse(t, minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"a\tb\"\n")
		err := CheckStart(&Loaded{Config: cfg, File: "test.toml"})
		requireErrorContains(t, err, "service.api.index.label: must not contain control characters")
	})

	t.Run("formats exactly what the old schema-level check printed", func(t *testing.T) {
		cfg := parse(t, minimalTOML+"[service.api]\n[service.api.index]\nlabel = \"a\tb\"\n")
		err := CheckStart(&Loaded{Config: cfg, File: "test.toml"})
		if err == nil {
			t.Fatal("CheckStart: expected an error")
		}
		want := "test.toml is not a valid berth.toml:\n  service.api.index.label: must not contain control characters"
		if got := err.Error(); got != want {
			t.Errorf("CheckStart error = %q, want %q", got, want)
		}
	})

	t.Run("reports both fields, and every service, in schema order", func(t *testing.T) {
		cfg := parse(t, minimalTOML+
			"[service.api]\n[service.api.index]\nlabel = \"a\tb\"\npurpose = \"c\td\"\n"+
			"[service.mail]\nsubdomain = \"mail\"\n[service.mail.index]\nlabel = \"e\tf\"\n")
		err := CheckStart(&Loaded{Config: cfg, File: "test.toml"})
		want := "test.toml is not a valid berth.toml:\n" +
			"  service.api.index.label: must not contain control characters\n" +
			"  service.api.index.purpose: must not contain control characters\n" +
			"  service.mail.index.label: must not contain control characters"
		if err == nil || err.Error() != want {
			t.Fatalf("CheckStart error = %v, want %q", err, want)
		}
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
		requireErrorContains(t, err, "Run 'berth init' at the root of the repository you want to manage to write one")
	})

	// Points at the file's permanent home on GitHub rather than a source
	// tree a release binary (built with no source tree alongside it)
	// doesn't have — true for a binary-only install today and once the
	// repository is public.
	t.Run("points the missing-config hint at the example file on GitHub, not a source tree a binary install lacks", func(t *testing.T) {
		root := t.TempDir()
		_, err := LoadNearest(root)
		if err == nil {
			t.Fatal("LoadNearest: expected an error, got none")
		}
		requireErrorContains(t, err, "the annotated version, berth.example.toml, is at "+
			"https://github.com/sofired/berth/blob/main/berth.example.toml.")
	})
}

// requireErrorContains fails the test unless err is non-nil and its message
// contains substring.
func requireErrorContains(t *testing.T, err error, substring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got none", substring)
	}
	if !strings.Contains(err.Error(), substring) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), substring)
	}
}
