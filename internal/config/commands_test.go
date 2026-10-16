package config

import (
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/text"
)

// parseCommands parses minimalTOML plus tail, failing the test on an error.
func parseCommands(t *testing.T, tail string) Commands {
	t.Helper()
	cfg, err := Parse([]byte(minimalTOML+tail), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg.Commands
}

// commandsError is the error message Parse gives minimalTOML plus tail.
func commandsError(t *testing.T, tail string) string {
	t.Helper()
	_, err := Parse([]byte(minimalTOML+tail), "test.toml")
	if err == nil {
		t.Fatalf("Parse accepted %q", tail)
	}
	return err.Error()
}

func TestCommands(t *testing.T) {
	t.Run("a file with no [commands] has none, and config show leaves the key out", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(cfg.Commands) != 0 {
			t.Fatalf("Commands = %#v, want none", cfg.Commands)
		}
		out, err := text.SafeJSON(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "commands") {
			t.Fatalf("config show printed commands:\n%s", out)
		}
	})

	t.Run("a bare string is the command, locked, with the default kill-after", func(t *testing.T) {
		got := parseCommands(t, "\n[commands]\nplan = \"./scripts/keycloak.sh plan\"\n")
		want := Command{Command: "./scripts/keycloak.sh plan", Lock: true, KillAfter: "10s"}
		if len(got) != 1 || got[0].Name != "plan" || got[0].Command != want {
			t.Fatalf("got %#v, want plan = %#v", got, want)
		}
	})

	t.Run("a table sets the command, lock and kill-after, and defaults what it leaves out", func(t *testing.T) {
		got := parseCommands(t, `
[commands.apply]
command    = "./scripts/keycloak.sh apply"
lock       = false
kill-after = "60s"

[commands.destroy]
command = "./scripts/keycloak.sh destroy"
`)
		apply, _ := got.Get("apply")
		if want := (Command{Command: "./scripts/keycloak.sh apply", Lock: false, KillAfter: "60s"}); apply != want {
			t.Errorf("apply = %#v, want %#v", apply, want)
		}
		destroy, _ := got.Get("destroy")
		if want := (Command{Command: "./scripts/keycloak.sh destroy", Lock: true, KillAfter: "10s"}); destroy != want {
			t.Errorf("destroy = %#v, want %#v", destroy, want)
		}
	})

	t.Run("an inline table works like a sub-table", func(t *testing.T) {
		got := parseCommands(t, "\n[commands]\napply = { command = \"./apply.sh\", lock = false }\n")
		apply, ok := got.Get("apply")
		if !ok || apply.Lock || apply.Command != "./apply.sh" {
			t.Fatalf("apply = %#v, %v", apply, ok)
		}
	})

	t.Run("keeps file order, mixing both forms, and config show prints an object in that order", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+`
[commands]
zeta = "./z.sh"
alpha = { command = "./a.sh", kill-after = "5m" }

[commands.mid]
command = "./m.sh"
`), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := strings.Join(cfg.Commands.Names(), ","); got != "zeta,alpha,mid" {
			t.Fatalf("Names = %s, want zeta,alpha,mid", got)
		}
		out, err := text.SafeJSON(cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := `  "commands": {
    "zeta": {
      "command": "./z.sh",
      "lock": true,
      "kill-after": "10s"
    },
    "alpha": {
      "command": "./a.sh",
      "lock": true,
      "kill-after": "5m"
    },
    "mid": {
      "command": "./m.sh",
      "lock": true,
      "kill-after": "10s"
    }
  }
}`
		if !strings.HasSuffix(out, ",\n"+want) {
			t.Fatalf("config show ended:\n%s\nwant it to end with\n%s", out, want)
		}
	})

	t.Run("an empty [commands] table is no commands", func(t *testing.T) {
		if got := parseCommands(t, "\n[commands]\n"); len(got) != 0 {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("names follow the service-name pattern", func(t *testing.T) {
		for _, name := range []string{"Plan", "my_cmd", "-plan", `"a.b"`, `"has space"`} {
			got := commandsError(t, "\n[commands]\n"+name+" = \"./x.sh\"\n")
			if !strings.Contains(got, "must be lower-case letters, digits, and hyphens") || !strings.Contains(got, "commands.") {
				t.Errorf("name %s: error = %q", name, got)
			}
		}
		if got := parseCommands(t, "\n[commands]\n0-plan = \"./x.sh\"\nplan-2 = \"./y.sh\"\n"); len(got) != 2 {
			t.Errorf("valid names rejected: %#v", got)
		}
	})

	t.Run("rejects a value that is neither a string nor a table", func(t *testing.T) {
		got := commandsError(t, "\n[commands]\nplan = 3\n")
		want := "test.toml is not a valid treerig.toml:\n  commands.plan: Invalid input: expected string or object, received number"
		if got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	})

	t.Run("rejects an empty command, as a string and as a table", func(t *testing.T) {
		for _, tail := range []string{"\n[commands]\nplan = \"\"\n", "\n[commands.plan]\ncommand = \"\"\n"} {
			if got := commandsError(t, tail); !strings.Contains(got, "Too small: expected string to have >=1 characters") {
				t.Errorf("%q: error = %q", tail, got)
			}
		}
	})

	t.Run("a table needs a command, and rejects keys it does not know", func(t *testing.T) {
		got := commandsError(t, "\n[commands.plan]\nlock = true\n")
		if want := "commands.plan.command: Invalid input: expected string, received undefined"; !strings.Contains(got, want) {
			t.Errorf("missing command: error = %q", got)
		}
		got = commandsError(t, "\n[commands.plan]\ncommand = \"./x.sh\"\nkillafter = \"5s\"\n")
		if want := "commands.plan: Unrecognized key: \"killafter\""; !strings.Contains(got, want) {
			t.Errorf("typo: error = %q", got)
		}
	})

	t.Run("lock must be a boolean", func(t *testing.T) {
		got := commandsError(t, "\n[commands.plan]\ncommand = \"./x.sh\"\nlock = \"no\"\n")
		if want := "commands.plan.lock: Invalid input: expected boolean, received string"; !strings.Contains(got, want) {
			t.Errorf("error = %q", got)
		}
	})

	t.Run("kill-after is a whole-number duration greater than zero", func(t *testing.T) {
		for _, good := range []string{"30s", "5m", "1m30s", "500ms", "1h", "59m59s"} {
			cmds := parseCommands(t, "\n[commands.plan]\ncommand = \"./x.sh\"\nkill-after = \""+good+"\"\n")
			plan, _ := cmds.Get("plan")
			if plan.KillAfter != good {
				t.Errorf("%s: kept %q", good, plan.KillAfter)
			}
		}
		for _, bad := range []string{"0s", "0", "30", "abc", "-5s", "1.5s", "5 m", "5d", "", "2h", "1h1ms", "999999999999h"} {
			got := commandsError(t, "\n[commands.plan]\ncommand = \"./x.sh\"\nkill-after = \""+bad+"\"\n")
			if want := "commands.plan.kill-after: must be a duration greater than zero and at most 1h, such as 30s, 5m or 1m30s"; !strings.Contains(got, want) {
				t.Errorf("%q: error = %q", bad, got)
			}
		}
	})

	t.Run("KillAfterDuration reads the spelling, and falls back to the default for a Command built by hand", func(t *testing.T) {
		if got := (Command{KillAfter: "1m30s"}).KillAfterDuration(); got != 90*time.Second {
			t.Errorf("1m30s = %v", got)
		}
		if got := (Command{}).KillAfterDuration(); got != 10*time.Second {
			t.Errorf("zero Command = %v, want 10s", got)
		}
	})

	t.Run("names are at most 63 characters, like a service's", func(t *testing.T) {
		long := strings.Repeat("a", 64)
		if got := commandsError(t, "\n[commands]\n"+long+" = \"./x.sh\"\n"); !strings.Contains(got, "must be at most 63 characters") {
			t.Errorf("64 characters: error = %q", got)
		}
		if got := parseCommands(t, "\n[commands]\n"+strings.Repeat("a", 63)+" = \"./x.sh\"\n"); len(got) != 1 {
			t.Errorf("63 characters rejected: %#v", got)
		}
	})

	t.Run("Get reports an unknown name", func(t *testing.T) {
		if _, ok := (Commands{}).Get("nope"); ok {
			t.Fatal("found a command in none")
		}
	})
}
