package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/cli"
)

// TestConfigSchemaPrintsTheCommittedSchema runs `config schema` through
// cli.Main, naming a config file that does not exist: the command reads no
// configuration, so it prints schema/treerig.schema.json regardless.
func TestConfigSchemaPrintsTheCommittedSchema(t *testing.T) {
	want, err := os.ReadFile("../../schema/treerig.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"treerig", "config", "schema", "--config", "/nonexistent-treerig-config-x7z.toml"}
	if code := cli.Main(args, strings.NewReader(""), &stdout, &stderr, emptyGetenv); code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	if stdout.String() != string(want) {
		t.Errorf("stdout differs from schema/treerig.schema.json; regenerate it with `go run ./cmd/treerig config schema > schema/treerig.schema.json`")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}
