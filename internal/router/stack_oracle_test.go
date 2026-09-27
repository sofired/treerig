package router_test

// Compares Go's stack document against a hand-maintained fixture
// (testdata/stack-oracle.json; its origin — recorded from TypeScript's own
// stack document for the same fixed inputs — is in git history). Through
// protocol 4 (WP3, #39) this was byte for byte apart from two changes: the
// protocol bump, and the dynamic/index mounts moving from under a control
// home to the machine-wide router state directory. Protocol 5 (#55) goes
// further — the docker-api service and network, and the router's own
// Docker-provider command line and depends_on, are dropped outright — so a
// byte-for-byte comparison of the whole document is no longer meaningful.
// This test instead compares only what still matches: the index service in
// full, and the router service's own image, restart, ports, hardening,
// tmpfs, volumes, healthcheck and labels, plus explicit assertions that the
// pieces #55 removed are in fact gone.
//
// #82 adds one more label, RouterHomeLabel, to both services — Go-only,
// since TypeScript has no berth-root ownership check to stamp a value for.
// It is asserted directly, then stripped from Go's own document before the
// comparisons above, so it never has to appear in the fixture at all.

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

// oldDynamicDir/oldIndexDir are the oracle fixture's own (pre-protocol-4,
// control-home-rooted) mount sources; newDynamicDir/newIndexDir are what
// TestStackOracle substitutes for them, and what it builds Go's own stack
// document with, to prove parity apart from the protocol-4 changes
// described above (Protocol's own comment).
const (
	oldDynamicDir = "/home/dev/.berth/router/dynamic"
	oldIndexDir   = "/home/dev/.berth/router/index"
	newDynamicDir = "/home/dev/.local/state/berth/router/dynamic"
	newIndexDir   = "/home/dev/.local/state/berth/router/index"
)

// marshalIndented is v encoded the way files.AtomicJSON encodes a stack
// document: no HTML escaping, two-space indent, trailing newline.
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

// objectAt is doc[key] as a map, failing the test with a clear message when
// it is missing or of another shape.
func objectAt(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	value, present := doc[key]
	if !present {
		t.Fatalf("missing %q", key)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object: %T", key, value)
	}
	return obj
}

func TestStackOracle(t *testing.T) {
	oracle, err := os.ReadFile("testdata/stack-oracle.json")
	if err != nil {
		t.Fatal(err)
	}

	oracleText := string(oracle)
	// The protocol-4 changes: every service's protocol label bumped (now to
	// 5, protocol 5's own bump), and the two mounts moved out from under a
	// control home.
	oracleText = strings.ReplaceAll(oracleText, `"dev.berth.router-protocol": "3"`, `"dev.berth.router-protocol": "`+strconv.Itoa(router.Protocol)+`"`)
	oracleText = strings.ReplaceAll(oracleText, oldDynamicDir, newDynamicDir)
	oracleText = strings.ReplaceAll(oracleText, oldIndexDir, newIndexDir)
	if oracleText == string(oracle) {
		t.Fatal("substitution changed nothing; the oracle fixture no longer matches this test's assumptions")
	}
	var oracleDoc map[string]any
	if err := json.Unmarshal([]byte(oracleText), &oracleDoc); err != nil {
		t.Fatalf("decoding the substituted oracle: %v", err)
	}

	// wantHome is this test's own fixture berth root, stamped as
	// router.RouterHomeLabel (#82) — a Go-only addition the fixture,
	// recorded from the frozen TypeScript build, never carries at all, so
	// it is asserted directly below and stripped from gotDoc before the
	// comparisons against the fixture that follow.
	const wantHome = "/home/dev/.berth"
	gotText, err := marshalIndented(router.Stack(router.StackOptions{
		ProjectName:    "berth-router",
		Network:        "berth-router",
		Port:           80,
		DynamicDir:     newDynamicDir,
		IndexDir:       newIndexDir,
		LabelNamespace: "dev.berth",
		Home:           wantHome,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var gotDoc map[string]any
	if err := json.Unmarshal([]byte(gotText), &gotDoc); err != nil {
		t.Fatalf("decoding Go's own stack document: %v", err)
	}

	oracleServices := objectAt(t, oracleDoc, "services")
	gotServices := objectAt(t, gotDoc, "services")

	for _, service := range []string{"index", "router"} {
		labels := objectAt(t, objectAt(t, gotServices, service), "labels")
		if got := labels[router.RouterHomeLabel]; got != wantHome {
			t.Errorf("%s.labels[%s] = %#v, want %q", service, router.RouterHomeLabel, got, wantHome)
		}
		delete(labels, router.RouterHomeLabel)
	}

	if !reflect.DeepEqual(gotServices["index"], oracleServices["index"]) {
		t.Errorf("index service does not match the fixture (plus the substitutions above).\n got: %#v\nwant: %#v",
			gotServices["index"], oracleServices["index"])
	}

	oracleRouter := objectAt(t, oracleServices, "router")
	gotRouter := objectAt(t, gotServices, "router")
	for _, field := range []string{"image", "restart", "ports", "read_only", "cap_drop", "security_opt", "cap_add", "tmpfs", "volumes", "healthcheck", "labels"} {
		if !reflect.DeepEqual(gotRouter[field], oracleRouter[field]) {
			t.Errorf("router.%s does not match the oracle:\n got: %#v\nwant: %#v", field, gotRouter[field], oracleRouter[field])
		}
	}

	// #55: no Docker-provider flag survives on the router's own command
	// line, it no longer depends on a docker-api healthcheck, and it is no
	// longer attached to the docker-api network — all deliberately
	// divergent from the oracle, not compared field for field above.
	command, ok := gotRouter["command"].([]any)
	if !ok {
		t.Fatalf("router.command is not an array: %#v", gotRouter["command"])
	}
	for _, arg := range command {
		if s, ok := arg.(string); ok && strings.Contains(s, "--providers.docker") {
			t.Errorf("router.command still names a Docker-provider flag: %v", arg)
		}
	}
	if _, present := gotRouter["depends_on"]; present {
		t.Error("router.depends_on is still present; #55 drops the docker-api dependency outright")
	}
	if got, want := gotRouter["networks"], []any{"router"}; !reflect.DeepEqual(got, want) {
		t.Errorf("router.networks = %v, want %v", got, want)
	}
	if _, present := gotServices["docker-api"]; present {
		t.Error("services.docker-api is still present; #55 drops it outright")
	}
	gotNetworks := objectAt(t, gotDoc, "networks")
	if _, present := gotNetworks["docker-api"]; present {
		t.Error("networks.docker-api is still present; #55 drops it outright")
	}
}
