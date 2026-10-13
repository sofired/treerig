package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uncommentExamples turns the starter's commented-out examples into live
// TOML, so a test can hold them to the schema: a block starts at a "# ["
// line and runs to the next line that is not a comment; inside it a table
// header or a "key = value" line loses its "# ", and prose stays a comment.
func uncommentExamples(starter string) string {
	var out []string
	live := false
	for _, line := range strings.Split(starter, "\n") {
		body := strings.TrimPrefix(line, "# ")
		switch {
		case strings.HasPrefix(line, "# ["):
			live = true
			out = append(out, body)
		case live && strings.HasPrefix(line, "# ") && strings.Contains(body, " = "):
			out = append(out, body)
		case live && strings.HasPrefix(line, "#"):
			out = append(out, line)
		default:
			live = false
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// squeeze is line with every run of white space made one space, to compare
// two spellings of a line that differ only in how they are aligned.
func squeeze(line string) string { return strings.Join(strings.Fields(line), " ") }

// starterOnly are the comment lines the starter may carry that
// berth.example.toml does not: the pointer to the example, and the notes that
// are about the starter itself. Keep it short; every line here is a place the
// starter can say something the reference does not.
var starterOnly = map[string]bool{
	"# hover text and unknown-key warnings.":                               true,
	"# The annotated reference, with every option, is berth.example.toml:": true,
	"#   " + exampleFileURL:                                                true,
	"# Placeholder: rename api to a service from your Compose file.":       true,
}

// TestStarter pins the file `init` writes to the schema and the example it
// is derived from.
func TestStarter(t *testing.T) {
	starter := string(Starter("acme", "docker/compose.yaml"))
	file := filepath.Join(t.TempDir(), "berth.toml")

	t.Run("parses and passes the start checks as written", func(t *testing.T) {
		cfg, err := Parse([]byte(starter), file)
		if err != nil {
			t.Fatalf("Parse: %v\n%s", err, starter)
		}
		if cfg.Project.Name != "acme" || cfg.Project.Domain != "acme.localhost" || cfg.Compose.File != "docker/compose.yaml" {
			t.Errorf("parsed project/compose = %+v / %+v, want acme, acme.localhost, docker/compose.yaml", cfg.Project, cfg.Compose)
		}
		if len(cfg.Service) != 0 || len(cfg.Hooks.Configured()) != 0 {
			t.Errorf("starter declares %d services and %v hooks, want none until the user uncomments them", len(cfg.Service), cfg.Hooks.Configured())
		}
		if err := CheckStart(&Loaded{Config: cfg, File: file, Root: filepath.Dir(file)}); err != nil {
			t.Errorf("CheckStart: %v", err)
		}
	})

	t.Run("keeps to TOML 1.0 syntax", func(t *testing.T) {
		for _, use := range toml11Uses(starter) {
			t.Errorf("uses TOML 1.1 syntax: %s", use)
		}
	})

	t.Run("opens with the same schema directive as the example", func(t *testing.T) {
		example, err := os.ReadFile(filepath.Join("..", "..", "berth.example.toml"))
		if err != nil {
			t.Fatal(err)
		}
		want, _, _ := strings.Cut(string(example), "\n")
		got, _, _ := strings.Cut(starter, "\n")
		if got != want {
			t.Errorf("first line = %q, want the example's %q", got, want)
		}
		if got != "#:schema "+SchemaID {
			t.Errorf("first line = %q, want the directive for SchemaID", got)
		}
	})

	t.Run("its commented-out examples are valid once uncommented", func(t *testing.T) {
		live := uncommentExamples(starter)
		if live == starter {
			t.Fatal("found no commented-out example to check; the extraction is broken")
		}
		cfg, err := Parse([]byte(live), file)
		if err != nil {
			t.Fatalf("Parse: %v\n%s", err, live)
		}
		if len(cfg.Service) != 1 || cfg.Service[0].Name != "api" || cfg.Service[0].Index == nil {
			t.Errorf("services = %+v, want one indexed service named api", cfg.Service)
		}
		if got := cfg.Hooks.Configured(); len(got) != 2 {
			t.Errorf("hooks = %v, want prepare and seed", got)
		}
		for _, use := range toml11Uses(live) {
			t.Errorf("uncommented, uses TOML 1.1 syntax: %s", use)
		}
	})

	t.Run("says nothing the example does not, bar a short list", func(t *testing.T) {
		example, err := os.ReadFile(filepath.Join("..", "..", "berth.example.toml"))
		if err != nil {
			t.Fatal(err)
		}
		exampleLines := map[string]bool{}
		for _, line := range strings.Split(string(example), "\n") {
			exampleLines[line] = true
		}
		// A commented-out line of the example's TOML may be spaced differently,
		// so that trailing comments line up under the "# " it gained.
		exampleFields := map[string]bool{}
		for line := range exampleLines {
			exampleFields[squeeze(line)] = true
		}
		seen := 0
		for _, line := range strings.Split(starter, "\n") {
			if !strings.HasPrefix(line, "#") || line == "#" || strings.HasPrefix(line, "#:schema ") {
				continue
			}
			seen++
			// A comment line of the example, or a line of its TOML commented out.
			if exampleLines[line] || (strings.HasPrefix(line, "# ") && exampleFields[squeeze(strings.TrimPrefix(line, "# "))]) || starterOnly[line] {
				continue
			}
			t.Errorf("comment line %q is not in berth.example.toml (commented out or not) or in starterOnly", line)
		}
		if seen == 0 {
			t.Fatal("found no comment line; the scan is broken")
		}
		used := 0
		for line := range starterOnly {
			if strings.Contains(starter, "\n"+line+"\n") {
				used++
			} else {
				t.Errorf("starterOnly line %q is not in the starter; remove it", line)
			}
		}
		if used != len(starterOnly) {
			t.Errorf("only %d of %d starterOnly lines are used", used, len(starterOnly))
		}
	})

	t.Run("names the annotated example", func(t *testing.T) {
		if !strings.Contains(starter, exampleFileURL) {
			t.Errorf("starter does not point at %s", exampleFileURL)
		}
	})
}

// TestStarterName pins the project name derived from a directory.
func TestStarterName(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"acme", "acme"},
		{"My_App 2", "my-app-2"},
		{"berth.feat-init", "berth-feat-init"},
		{"--leading--and--trailing--", "leading-and-trailing"},
		{"Ünïcode-café", "n-code-caf"},
		{"日本語", "my-project"},
		{"", "my-project"},
		{"---", "my-project"},
		{strings.Repeat("a", 100), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + "-bbbb", strings.Repeat("a", 39)},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			got := StarterName(tc.dir)
			if got != tc.want {
				t.Errorf("StarterName(%q) = %q, want %q", tc.dir, got, tc.want)
			}
			if _, err := Parse(Starter(got, "compose.yaml"), filepath.Join(t.TempDir(), "berth.toml")); err != nil {
				t.Errorf("a starter named %q does not parse: %v", got, err)
			}
		})
	}
}
