package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/cli"
)

// TestConfigSchemaPrintsTheCommittedSchema runs `config schema` through
// cli.Main with no configuration to read: the command reads none (and takes no
// --config), so it prints schema/treerig.schema.json wherever it runs.
func TestConfigSchemaPrintsTheCommittedSchema(t *testing.T) {
	want, err := os.ReadFile("../../schema/treerig.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"treerig", "config", "schema"}
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
