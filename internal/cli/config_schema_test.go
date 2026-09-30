package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/cli"
)

// TestConfigSchemaPrintsTheCommittedSchema runs `config schema` through
// cli.Main, naming a config file that does not exist: the command reads no
// configuration, so it prints schema/berth.schema.json regardless.
func TestConfigSchemaPrintsTheCommittedSchema(t *testing.T) {
	want, err := os.ReadFile("../../schema/berth.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"berth", "config", "schema", "--config", "/nonexistent-berth-config-x7z.toml"}
	if code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, emptyGetenv); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if stdout.String() != string(want) {
		t.Errorf("stdout differs from schema/berth.schema.json; regenerate it with `go run ./cmd/berth config schema > schema/berth.schema.json`")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}
