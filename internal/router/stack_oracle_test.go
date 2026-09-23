package router_test

// Compares Go's stack document against TypeScript's for the same fixed
// inputs, byte for byte apart from the two changes WP3 (#39) ledgers: the
// protocol bumped from 3 to 4, and the dynamic/index mounts moved from under
// a control home to the machine-wide router state directory. See
// tools/oracle/router-stack.ts's header for how testdata/stack-oracle.json
// is regenerated, and docs/port/divergences.md for the ledger rows.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

const (
	oldDynamicDir = "/home/dev/.berth/router/dynamic"
	oldIndexDir   = "/home/dev/.berth/router/index"
	newDynamicDir = "/home/dev/.local/state/berth/router/dynamic"
	newIndexDir   = "/home/dev/.local/state/berth/router/index"
)

func marshalIndented(v any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func TestStackOracle(t *testing.T) {
	oracle, err := os.ReadFile("testdata/stack-oracle.json")
	if err != nil {
		t.Fatal(err)
	}

	want := string(oracle)
	// The ledgered protocol-4 changes (docs/port/divergences.md): every
	// service's protocol label bumped, and the two mounts moved out from
	// under a control home.
	want = strings.ReplaceAll(want, `"dev.berth.router-protocol": "3"`, `"dev.berth.router-protocol": "4"`)
	want = strings.ReplaceAll(want, oldDynamicDir, newDynamicDir)
	want = strings.ReplaceAll(want, oldIndexDir, newIndexDir)
	if want == string(oracle) {
		t.Fatal("substitution changed nothing; the oracle fixture no longer matches this test's assumptions")
	}

	got, err := marshalIndented(router.Stack(router.StackOptions{
		ProjectName:    "berth-router",
		Network:        "berth-router",
		Port:           80,
		DynamicDir:     newDynamicDir,
		IndexDir:       newIndexDir,
		LabelNamespace: "dev.berth",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Errorf("stack document does not match the oracle plus the ledgered protocol-4 changes.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
