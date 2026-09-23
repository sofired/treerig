package router_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

func TestRouterServiceVerify(t *testing.T) {
	// The 19-row tampering table, tests/unit/router.test.ts:599-619.
	tampering := []struct {
		name     string
		tamper   func(router, api, index *router.ContainerInspect)
		expected string
	}{
		{
			"the index serving another directory",
			func(_, _, index *router.ContainerInspect) {
				index.Mounts = []router.InspectMount{{Type: "bind", Source: "/etc", Destination: router.IndexMount, RW: false}}
			},
			"index: mounts are not exactly the read-only index directory",
		},
		{
			"the index publishing a port",
			func(_, _, index *router.ContainerInspect) {
				index.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}}}
			},
			"index: publishes a port",
		},
		{
			"the index routed elsewhere",
			func(_, _, index *router.ContainerInspect) {
				index.Config.Labels["traefik.http.routers.berth-index.rule"] = "Host(`evil.localhost`)"
			},
			"index: routing labels differ",
		},
		{
			"a router still carrying an older protocol label",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.Config.Labels = testLabels("router")
				routerDoc.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol - 1)
			},
			"router: protocol label",
		},
		{
			"a writable root filesystem",
			func(routerDoc, _, _ *router.ContainerInspect) { routerDoc.HostConfig.ReadonlyRootfs = false },
			"router: root filesystem is writable",
		},
		{
			"kept capabilities",
			func(routerDoc, _, _ *router.ContainerInspect) { routerDoc.HostConfig.CapDrop = nil },
			"router: capabilities are not dropped",
		},
		{
			"extra capabilities",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.HostConfig.CapAdd = []string{"CAP_NET_BIND_SERVICE", "CAP_SYS_ADMIN"}
			},
			"router: capabilities beyond NET_BIND_SERVICE",
		},
		{
			"a port on every interface",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "80"}}}
			},
			"published only on 127.0.0.1:80",
		},
		{
			"an extra published port",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.NetworkSettings.Ports["8080/tcp"] = []router.InspectPortBinding{{HostIP: "127.0.0.1", HostPort: "8080"}}
			},
			"unexpected ports (8080/tcp)",
		},
		{
			"the file provider removed",
			func(routerDoc, _, _ *router.ContainerInspect) {
				var filtered []string
				for _, arg := range router.RouterCommand(testNetwork) {
					if !strings.HasPrefix(arg, "--providers.file") {
						filtered = append(filtered, arg)
					}
				}
				routerDoc.Config.Cmd = filtered
			},
			"router: command line differs",
		},
		{
			"a different image",
			func(routerDoc, _, _ *router.ContainerInspect) { routerDoc.Config.Image = "traefik:latest" },
			"router: image is not the pinned digest",
		},
		{
			"an extra mount",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.Mounts = append(routerDoc.Mounts, router.InspectMount{Type: "bind", Source: "/etc", Destination: "/host-etc", RW: false})
			},
			"router: mounts are not exactly",
		},
		{
			"a writable dynamic directory",
			func(routerDoc, _, _ *router.ContainerInspect) {
				for i := range routerDoc.Mounts {
					routerDoc.Mounts[i].RW = true
				}
			},
			"router: mounts are not exactly",
		},
		{
			"an unhealthy router",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.State = router.InspectState{Status: "running", Health: router.InspectHealth{Status: "unhealthy"}}
			},
			"router: not healthy",
		},
		{
			"a foreign container",
			func(routerDoc, _, _ *router.ContainerInspect) {
				routerDoc.Config.Labels = testLabels("router")
				routerDoc.Config.Labels["com.docker.compose.project"] = "other"
			},
			"router: not the container Compose started for this project",
		},
		{
			"POST allowed through the proxy",
			func(_, api, _ *router.ContainerInspect) {
				for i, entry := range api.Config.Env {
					if entry == "POST=0" {
						api.Config.Env[i] = "POST=1"
					}
				}
			},
			"docker-api: API access differs (POST)",
		},
		{
			"a writable socket",
			func(_, api, _ *router.ContainerInspect) {
				for i := range api.Mounts {
					api.Mounts[i].RW = true
				}
			},
			"docker-api: mounts are not exactly the read-only Docker socket",
		},
		{
			"the proxy on the shared network",
			func(_, api, _ *router.ContainerInspect) {
				api.NetworkSettings.Networks[testNetwork] = json.RawMessage("{}")
			},
			"docker-api: attached to a network other than the internal proxy network",
		},
		{
			"the proxy publishing a port",
			func(_, api, _ *router.ContainerInspect) {
				api.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"2375/tcp": {{HostIP: "127.0.0.1", HostPort: "2375"}}}
			},
			"docker-api: publishes a port",
		},
	}

	for _, tc := range tampering {
		t.Run("reports "+tc.name, func(t *testing.T) {
			w := newWorld(t, 80, "")
			routerDoc := routerDoc(w.State.DynamicDir, w.Port)
			apiDoc := apiDoc()
			indexDoc := indexDoc(w.State.IndexDir)
			tc.tamper(&routerDoc, &apiDoc, &indexDoc)
			w.Fake.setDoc("router-1", routerDoc)
			w.Fake.setDoc("api-1", apiDoc)
			w.Fake.setDoc("index-1", indexDoc)

			problems, err := w.Router.Verify(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly one", problems)
			}
			if !strings.Contains(problems[0], tc.expected) {
				t.Errorf("problem = %q, want it to contain %q", problems[0], tc.expected)
			}
		})
	}

	t.Run("is clean for a router exactly as the stack asked for it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 0 {
			t.Errorf("problems = %v, want none", problems)
		}
	})
}
