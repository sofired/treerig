package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/sofired/berth/internal/shape"
)

// sharedTOML is the [shared] table every shared-service case needs ahead of
// its [shared.service.<name>] tables.
const sharedTOML = "[shared]\nfile = \"compose.shared.yaml\"\n"

// schemaFile is the committed JSON Schema, relative to this package.
const schemaFile = "../../schema/berth.schema.json"

// TestSchemaFileIsCurrent fails when schema/berth.schema.json is not what
// `berth config schema` prints, and says how to bring it back in line.
func TestSchemaFileIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("reading %s: %v\nRegenerate it with: go run ./cmd/berth config schema > schema/berth.schema.json", schemaFile, err)
	}
	if generated := SchemaJSON(); !bytes.Equal(committed, generated) {
		t.Errorf("schema/berth.schema.json differs from the generated schema.\n"+
			"Regenerate it from the repository root with:\n\n\tgo run ./cmd/berth config schema > schema/berth.schema.json\n\n"+
			"and review the diff. Committed %d bytes, generated %d bytes.", len(committed), len(generated))
	}
}

// TestSchemaJSON checks what the generated schema promises editors.
func TestSchemaJSON(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(SchemaJSON(), &root); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}

	t.Run("is draft-07 and names the file", func(t *testing.T) {
		if got := root["$schema"]; got != "http://json-schema.org/draft-07/schema#" {
			t.Errorf("$schema = %v", got)
		}
		if got := root["$id"]; got != SchemaID {
			t.Errorf("$id = %v, want %s", got, SchemaID)
		}
		if got := root["title"]; got != "berth.toml" {
			t.Errorf("title = %v", got)
		}
	})

	t.Run("covers every table of the file", func(t *testing.T) {
		properties := root["properties"].(map[string]any)
		var got []string
		for name := range properties {
			got = append(got, name)
		}
		sort.Strings(got)
		if want := "commands,compose,hooks,project,service,shared"; strings.Join(got, ",") != want {
			t.Errorf("top-level keys = %v, want %s", got, want)
		}
		service := properties["service"].(map[string]any)
		entry := service["additionalProperties"].(map[string]any)
		if _, ok := entry["properties"].(map[string]any)["name"]; ok {
			t.Error("a service table has a name property; its key is the name")
		}
		if entry["additionalProperties"] != false {
			t.Error("a service table accepts unknown keys")
		}
		commands := properties["commands"].(map[string]any)
		alternatives := commands["additionalProperties"].(map[string]any)["anyOf"].([]any)
		if len(alternatives) != 2 {
			t.Errorf("a commands entry has %d alternatives, want the string and the table", len(alternatives))
		}
	})

	t.Run("states which keys are required", func(t *testing.T) {
		if got := root["required"]; !equalStrings(got, "project", "compose") {
			t.Errorf("required = %v", got)
		}
		project := root["properties"].(map[string]any)["project"].(map[string]any)
		if got := project["required"]; !equalStrings(got, "name", "domain") {
			t.Errorf("project.required = %v", got)
		}
	})

	t.Run("gives every field a description", func(t *testing.T) {
		var missing []string
		var walk func(path string, node map[string]any)
		walk = func(path string, node map[string]any) {
			if d, _ := node["description"].(string); strings.TrimSpace(d) == "" {
				missing = append(missing, path)
			}
			if properties, ok := node["properties"].(map[string]any); ok {
				for name, child := range properties {
					walk(path+"."+name, child.(map[string]any))
				}
			}
			if child, ok := node["additionalProperties"].(map[string]any); ok {
				walk(path+".*", child)
			}
			if child, ok := node["propertyNames"].(map[string]any); ok {
				if child["type"] != "string" {
					missing = append(missing, path+".<key> (no type)")
				}
				walk(path+".<key>", child)
			}
			if child, ok := node["items"].(map[string]any); ok {
				walk(path+"[]", child)
			}
			if alternatives, ok := node["anyOf"].([]any); ok {
				for i, child := range alternatives {
					walk(path+"|"+string(rune('0'+i)), child.(map[string]any))
				}
			}
		}
		walk("(root)", root)
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("no description for: %s", strings.Join(missing, ", "))
		}
	})

	t.Run("is plain ASCII, so it reads the same in any terminal", func(t *testing.T) {
		for i, b := range SchemaJSON() {
			if b >= 0x7f || (b < 0x20 && b != '\n') {
				t.Fatalf("byte %d is %#x", i, b)
			}
		}
	})
}

// equalStrings reports whether v is a JSON array of exactly want.
func equalStrings(v any, want ...string) bool {
	list, ok := v.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, item := range list {
		if item != want[i] {
			return false
		}
	}
	return true
}

// TestCodeOnlyChecks pins where validation checks something the published
// schema cannot state, so adding a Refine or SuperRefine is a visible choice:
// either restate it with SchemaPattern, or add it here, and to the README's
// note that berth config validate makes the checks the schema cannot.
func TestCodeOnlyChecks(t *testing.T) {
	want := []string{
		"(root)",         // two services claiming the same route
		"project.domain", // each label at most 63 characters
	}
	if got := shape.CodeOnlyChecks(rootSchema("")); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CodeOnlyChecks = %v, want %v", got, want)
	}
}

// TestSchemaAgreesWithValidation compiles the committed schema as draft-07,
// with a JSON Schema library that shares no code with berth, and checks that
// it and Parse reach the same verdict on each document. The cases where the
// schema is looser are the code-only checks TestCodeOnlyChecks lists, and
// kill-after's and a hook timeout's range; it is never stricter.
func TestSchemaAgreesWithValidation(t *testing.T) {
	committed, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(committed))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaID, doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(SchemaID)
	if err != nil {
		t.Fatalf("the schema does not compile as draft-07: %v", err)
	}

	// schemaAccepts is the library's verdict on a TOML document.
	schemaAccepts := func(t *testing.T, source string) bool {
		t.Helper()
		var decoded map[string]any
		if _, err := toml.Decode(source, &decoded); err != nil {
			t.Fatalf("the case is not TOML: %v", err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		return compiled.Validate(instance) == nil
	}

	example, err := os.ReadFile(filepath.Join("..", "..", "berth.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	services := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "[service.s%d]\nsubdomain = \"s%d\"\n", i, i)
		}
		return b.String()
	}

	// portList is n distinct ports as a TOML array.
	portList := func(n int) string {
		ports := make([]string, n)
		for i := range ports {
			ports[i] = strconv.Itoa(1000 + i)
		}
		return "[" + strings.Join(ports, ", ") + "]"
	}

	type verdict int
	const (
		valid       verdict = iota // both accept
		invalid                    // both reject
		schemaLoose                // Parse rejects a check the schema cannot state
	)
	cases := []struct {
		name string
		toml string
		want verdict
	}{
		{"the example file", string(example), valid},
		{"the minimal file", minimalTOML, valid},
		{"a fully spelled service", minimalTOML + "[service.api]\nsubdomain = \"a\"\npath = \"/x\"\nport = 80\n[service.api.index]\nlabel = \"A\"\npurpose = \"p\"\n", valid},
		{"two services sharing a host by path", minimalTOML + "[service.a]\npath = \"/a\"\n[service.b]\npath = \"/b\"\n", valid},
		{"commands in both forms", minimalTOML + "[commands]\nplan = \"./p.sh\"\n[commands.apply]\ncommand = \"./a.sh\"\nlock = false\nkill-after = \"1m30s\"\n", valid},
		{"all hooks", minimalTOML + "[hooks]\nprepare = \"a\"\nseed = \"b\"\nverify = \"c\"\nbackup = \"d\"\nrestore = \"e\"\n", valid},
		{"64 services", minimalTOML + services(64), valid},
		{"a fully spelled shared service", minimalTOML + sharedTOML + "[shared.service.mailpit]\nport = 8025\nports = [1025, 1110]\nused-by = [\"web\", \"worker\"]\n", valid},
		{"a shared file with no service", minimalTOML + sharedTOML, valid},
		{"a shared service with nothing but its table", minimalTOML + sharedTOML + "[shared.service.db]\n", valid},
		{"an unknown top-level key", minimalTOML + "extra = 1\n", invalid},
		{"an unknown key in [shared]", minimalTOML + sharedTOML + "bogus = 1\n", invalid},
		{"an unknown key in a shared service", minimalTOML + sharedTOML + "[shared.service.db]\nbogus = 1\n", invalid},
		{"a name inside a shared service", minimalTOML + sharedTOML + "[shared.service.db]\nname = \"db\"\n", invalid},
		{"a shared service name that is not a slug", minimalTOML + sharedTOML + "[shared.service.Db]\n", invalid},
		{"a shared port of 0", minimalTOML + sharedTOML + "[shared.service.db]\nport = 0\n", invalid},
		{"a shared raw port of 65536", minimalTOML + sharedTOML + "[shared.service.db]\nports = [65536]\n", invalid},
		{"a shared raw port that is a string", minimalTOML + sharedTOML + "[shared.service.db]\nports = [\"25\"]\n", invalid},
		{"seventeen shared raw ports", minimalTOML + sharedTOML + "[shared.service.db]\nports = " + portList(17) + "\n", invalid},
		{"sixteen shared raw ports", minimalTOML + sharedTOML + "[shared.service.db]\nports = " + portList(16) + "\n", valid},
		{"a used-by that is not a slug", minimalTOML + sharedTOML + "[shared.service.db]\nused-by = [\"Web\"]\n", invalid},
		{"an empty shared file", minimalTOML + "[shared]\nfile = \"\"\n", invalid},
		{"shared services as an array of tables", minimalTOML + sharedTOML + "[[shared.service]]\nname = \"db\"\n", invalid},
		{"shared as a string", strings.Replace(minimalTOML, "[compose]", "shared = \"x\"\n[compose]", 1), invalid},
		{"a shared service without a shared file", minimalTOML + "[shared.service.db]\n", schemaLoose},
		{"the shared file being the compose file", minimalTOML + "[shared]\nfile = \"docker/compose.yaml\"\n[shared.service.db]\n", schemaLoose},
		{"a shared service named like a worktree service", minimalTOML + sharedTOML + "[service.db]\n[shared.service.db]\n", schemaLoose},
		{"a used-by listed twice", minimalTOML + sharedTOML + "[shared.service.db]\nused-by = [\"web\", \"web\"]\n", schemaLoose},
		{"a used-by naming a shared service", minimalTOML + sharedTOML + "[shared.service.db]\n[shared.service.mail]\nused-by = [\"db\"]\n", schemaLoose},
		{"a port repeated in ports", minimalTOML + sharedTOML + "[shared.service.db]\nports = [25, 25]\n", schemaLoose},
		{"port also listed in ports", minimalTOML + sharedTOML + "[shared.service.db]\nport = 25\nports = [25]\n", schemaLoose},
		{"seventeen ports between port and ports", minimalTOML + sharedTOML + "[shared.service.db]\nport = 9000\nports = " + portList(16) + "\n", schemaLoose},
		{"an unknown key in a service", minimalTOML + "[service.api]\nbogus = 1\n", invalid},
		{"a name inside a service", minimalTOML + "[service.api]\nname = \"api\"\n", invalid},
		{"an unknown key in a hooks table", minimalTOML + "[hooks]\nbogus = \"x\"\n", invalid},
		{"an unknown key in a command", minimalTOML + "[commands.a]\ncommand = \"x\"\nbogus = 1\n", invalid},
		{"a service name that is not a slug", minimalTOML + "[service.Api]\n", invalid},
		{"a service name of 64 characters", minimalTOML + "[service." + strings.Repeat("a", 64) + "]\n", invalid},
		{"a service name of 63 characters", minimalTOML + "[service." + strings.Repeat("a", 63) + "]\n", valid},
		{"a command name that is not a slug", minimalTOML + "[commands]\nBad = \"x\"\n", invalid},
		{"65 services", minimalTOML + services(65), invalid},
		{"a missing project name", "[project]\ndomain = \"acme.localhost\"\n[compose]\nfile = \"c.yaml\"\n", invalid},
		{"a missing compose table", "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n", invalid},
		{"a port of 0", minimalTOML + "[service.a]\nport = 0\n", invalid},
		{"a port of 65536", minimalTOML + "[service.a]\nport = 65536\n", invalid},
		{"a port that is a string", minimalTOML + "[service.a]\nport = \"80\"\n", invalid},
		{"a port that is fractional", minimalTOML + "[service.a]\nport = 80.5\n", invalid},
		{"a path without a leading slash", minimalTOML + "[service.a]\npath = \"x\"\n", invalid},
		{"a subdomain starting with a hyphen", minimalTOML + "[service.a]\nsubdomain = \"-x\"\n", invalid},
		{"an empty index label", minimalTOML + "[service.a]\n[service.a.index]\nlabel = \"\"\n", invalid},
		{"a bad label namespace", "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\nlabel-namespace = \"nope\"\n[compose]\nfile = \"c.yaml\"\n", invalid},
		{"a kill-after that is not a duration", minimalTOML + "[commands.a]\ncommand = \"x\"\nkill-after = \"soon\"\n", invalid},
		{"an empty hook", minimalTOML + "[hooks]\nprepare = \"\"\n", invalid},
		{"hook timeouts", minimalTOML + "[hooks]\nprepare = \"x\"\n[hooks.timeout]\nprepare = \"10m\"\nverify = \"90s\"\nrestore = \"1h30m\"\n", valid},
		{"a hook timeout that is not a duration", minimalTOML + "[hooks.timeout]\nprepare = \"soon\"\n", invalid},
		{"a hook timeout that is a number", minimalTOML + "[hooks.timeout]\nprepare = 600\n", invalid},
		{"a timeout for a hook that does not exist", minimalTOML + "[hooks.timeout]\nmigrate = \"1m\"\n", invalid},
		{"services as an array of tables", minimalTOML + "[[service]]\nname = \"api\"\n", invalid},
		{"services as a string", strings.Replace(minimalTOML, "[compose]", "service = \"x\"\n[compose]", 1), invalid},
		{"two services on the same route", minimalTOML + "[service.a]\n[service.b]\n", schemaLoose},
		{"a kill-after of zero", minimalTOML + "[commands.a]\ncommand = \"x\"\nkill-after = \"0s\"\n", schemaLoose},
		{"a hook timeout of zero", minimalTOML + "[hooks.timeout]\nprepare = \"0s\"\n", schemaLoose},
		{"a hook timeout over a day", minimalTOML + "[hooks.timeout]\nprepare = \"25h\"\n", schemaLoose},
		{"a kill-after over an hour", minimalTOML + "[commands.a]\ncommand = \"x\"\nkill-after = \"2h\"\n", schemaLoose},
		{"a domain label over 63 characters", "[project]\nname = \"acme\"\ndomain = \"" + strings.Repeat("a", 64) + ".localhost\"\n[compose]\nfile = \"c.yaml\"\n", schemaLoose},
		{"a purpose of 300 emoji: 300 code points, 600 UTF-16 units", minimalTOML + "[service.a]\n[service.a.index]\nlabel = \"A\"\npurpose = \"" + strings.Repeat("\U0001F600", 300) + "\"\n", schemaLoose},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, parseErr := Parse([]byte(c.toml), "test.toml")
			parseAccepts := parseErr == nil
			schemaVerdict := schemaAccepts(t, c.toml)
			wantParse := c.want == valid
			wantSchema := c.want != invalid
			if parseAccepts != wantParse {
				t.Errorf("Parse accepts = %v, want %v (error: %v)", parseAccepts, wantParse, parseErr)
			}
			if schemaVerdict != wantSchema {
				t.Errorf("schema accepts = %v, want %v", schemaVerdict, wantSchema)
			}
		})
	}
}
