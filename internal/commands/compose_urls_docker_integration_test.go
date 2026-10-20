//go:build integration

package commands_test

// The routed URLs reach compose.yaml (#177) on a real daemon: a Compose file
// that interpolates ${TREERIG_URL} and ${TREERIG_SERVICE_URL_<NAME>} starts
// containers that see exactly the URLs `treerig urls` prints, the router's
// (non-default) port included, and a second `up` over the same inputs leaves
// them alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
)

// composeURLsCompose reads the worktree's routed URLs into each container's
// environment. mail-relay checks the hyphen-to-underscore name mapping.
const composeURLsCompose = `services:
  web:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from web' > /www/index.html && exec httpd -f -p 8080 -h /www"]
    expose: ["8080"]
    environment:
      PUBLIC_URL: "${TREERIG_URL:?no service is routed at the host root}"
      WEB_URL: "${TREERIG_SERVICE_URL_WEB}"
  mail-relay:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo 'hello from relay' > /www/index.html && exec httpd -f -p 9090 -h /www"]
    expose: ["9090"]
    environment:
      RELAY_URL: "${TREERIG_SERVICE_URL_MAIL_RELAY}"
`

// composeURLsToml is treerig.toml for composeURLsCompose: web at the host
// root, mail-relay on the "mail" subdomain. %s takes the run's label namespace.
const composeURLsToml = `[project]
name = "composeurls"
domain = "composeurls.localhost"
label-namespace = %q

[compose]
file = "compose.yaml"

[service.web]

[service.mail-relay]
subdomain = "mail"
`

// containerEnv is the environment of the one container the worktree's project
// runs for service, from `docker inspect`.
func containerEnv(t *testing.T, project, service string) (id string, env map[string]string) {
	t.Helper()
	ids := dockerIDs(t, "compose", "-p", project, "ps", "-q", service)
	if len(ids) != 1 {
		t.Fatalf("%s container ids = %v", service, ids)
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}", ids[0]).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", ids[0], err)
	}
	var entries []string
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("decoding %s: %v", out, err)
	}
	env = map[string]string{}
	for _, entry := range entries {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return ids[0], env
}

func TestComposeInterpolatesTheRoutedURLsOnARealDaemon(t *testing.T) {
	w := newWalkFixture(t, "composeurls", "feature/urls", composeURLsToml, composeURLsCompose)
	const host = "feature-urls.composeurls.localhost"

	result := w.up(t)
	w.eventually(t, host, func(status int, body string) bool { return status == 200 && body == "hello from web\n" })

	// What `treerig urls` prints for this worktree, with the walk's router port.
	listed, err := commands.UrlsWithContext(context.Background(), w.context(t), commands.OutputOptions{})
	if err != nil {
		t.Fatalf("urls: %v", err)
	}
	urls := map[string]string{}
	for _, entry := range listed {
		urls[entry.Service] = entry.URL
	}
	if w.port == 80 || !strings.HasSuffix(urls["web"], fmt.Sprintf(":%d", w.port)) {
		t.Fatalf("test setup: urls = %v with router port %d", urls, w.port)
	}

	project := layout.ComposeProjectName(result.Registration.RepositoryID, result.Registration.WorktreeID)
	webID, web := containerEnv(t, project, "web")
	relayID, relay := containerEnv(t, project, "mail-relay")
	for name, want := range map[string]string{"PUBLIC_URL": urls["web"], "WEB_URL": urls["web"]} {
		if web[name] != want {
			t.Errorf("web sees %s = %q, treerig urls prints %q", name, web[name], want)
		}
	}
	if relay["RELAY_URL"] != urls["mail-relay"] || urls["mail-relay"] == "" {
		t.Errorf("mail-relay sees RELAY_URL = %q, treerig urls prints %q", relay["RELAY_URL"], urls["mail-relay"])
	}

	// The same inputs interpolate the same, so a second up recreates nothing.
	w.up(t)
	if again, _ := containerEnv(t, project, "web"); again != webID {
		t.Errorf("web was recreated by an up with identical inputs: %s -> %s", webID, again)
	}
	if again, _ := containerEnv(t, project, "mail-relay"); again != relayID {
		t.Errorf("mail-relay was recreated by an up with identical inputs: %s -> %s", relayID, again)
	}

	w.downAndRemove(t, result, host)
}
