package router_test

// Compares the stack document against hand-maintained fixtures, one per
// mode and privilege level (testdata/stack-golden-*.json), byte for byte,
// plus explicit assertions on the properties the design rests on: nothing
// listens on a wildcard address, nothing publishes the ping, the proxy has
// no Docker socket and no Docker-provider flag, and no network is declared.
//
// `go test ./internal/router -run TestStackGolden -update` rewrites the
// fixtures from what Stack builds; it never judges the result, so read every
// diff it leaves.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

// updateGoldens rewrites the stack fixtures from the current output.
var updateGoldens = flag.Bool("update", false, "rewrite the stack golden files")

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

// goldenOptions are the options a golden is built from: fixed paths, ports
// and berth root, so the fixture pins the document's shape and not where one
// machine keeps its router state.
func goldenOptions(mode router.Mode, port int) router.StackOptions {
	return router.StackOptions{
		Mode: mode, Port: port, IndexPort: 21000, PingPort: 21001,
		DynamicDir: "/home/dev/.berth/router/dynamic", IndexDir: "/home/dev/.berth/router/index",
		LabelNamespace: "dev.berth", Home: "/home/dev/.berth",
	}
}

func TestStackGolden(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		options router.StackOptions
	}{
		{"host mode on a privileged port runs as root with NET_BIND_SERVICE", "stack-golden-host-privileged.json", goldenOptions(router.ModeHost, 80)},
		{"host mode on an unprivileged port runs as nobody with no capability", "stack-golden-host-unprivileged.json", goldenOptions(router.ModeHost, 8080)},
		{"bridge mode publishes the router port on loopback and keeps the ping inside", "stack-golden-bridge.json", goldenOptions(router.ModeBridge, 8080)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := marshalIndented(router.Stack(tc.options))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tc.file)
			if *updateGoldens {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("stack document differs from %s:\n got:\n%s\nwant:\n%s", path, got, want)
			}
		})
	}
}

func TestStackProperties(t *testing.T) {
	for name, options := range map[string]router.StackOptions{
		"host, privileged":   goldenOptions(router.ModeHost, 80),
		"host, unprivileged": goldenOptions(router.ModeHost, 8080),
		"bridge":             goldenOptions(router.ModeBridge, 8080),
	} {
		t.Run(name+": nothing is wildcard-bound, ping is on its own loopback entrypoint, and no network or socket is in the document", func(t *testing.T) {
			text, err := marshalIndented(router.Stack(options))
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(text), &doc); err != nil {
				t.Fatal(err)
			}
			if _, present := doc["networks"]; present {
				t.Error("the stack declares top-level networks")
			}
			services := doc["services"].(map[string]any)
			for serviceName, raw := range services {
				service := raw.(map[string]any)
				if _, present := service["networks"]; present {
					t.Errorf("%s joins a named network", serviceName)
				}
				if _, present := service["depends_on"]; present {
					t.Errorf("%s depends on another service", serviceName)
				}
			}
			if strings.Contains(text, "docker.sock") || strings.Contains(text, "--providers.docker") {
				t.Error("the stack touches the Docker socket or Docker provider")
			}

			command := router.RouterCommand(options)
			var ping, web string
			for _, argument := range command {
				if value, ok := strings.CutPrefix(argument, "--entrypoints.traefik.address="); ok {
					ping = value
				}
				if value, ok := strings.CutPrefix(argument, "--entrypoints.web.address="); ok {
					web = value
				}
			}
			if !strings.HasPrefix(ping, "127.0.0.1:") {
				t.Errorf("ping entrypoint = %q, want a loopback address", ping)
			}
			if !containsSubstring(command, "--ping.entryPoint=traefik") {
				t.Errorf("ping is not on its own entrypoint: %v", command)
			}
			if options.Mode == router.ModeHost && !strings.HasPrefix(web, "127.0.0.1:") {
				t.Errorf("host-mode web entrypoint = %q, want a loopback address", web)
			}
			if strings.Contains(strings.Join(command, " "), "8080") && options.Port != 8080 {
				t.Errorf("command names Traefik's default :8080: %v", command)
			}
			router := services["router"].(map[string]any)
			health := strings.Join(toStrings(router["healthcheck"].(map[string]any)["test"]), " ")
			if !strings.Contains(health, "--entrypoints.traefik.address="+ping) {
				t.Errorf("healthcheck %q does not carry the ping address %q", health, ping)
			}
			if ports, present := router["ports"]; present && strings.Contains(strings.Join(toStrings(ports), " "), ping) {
				t.Errorf("the ping is published: %v", ports)
			}
		})
	}
}

// toStrings is v, a decoded JSON array of strings, as a slice.
func toStrings(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}
