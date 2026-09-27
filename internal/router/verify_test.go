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
	// One row per way a running container can differ from what the stack
	// asked for; each must be reported.
	tampering := []struct {
		name     string
		tamper   func(routerDoc, indexDoc *router.ContainerInspect)
		expected string
	}{
		{
			"the index serving another directory",
			func(_, index *router.ContainerInspect) {
				index.Mounts = []router.InspectMount{{Type: "bind", Source: "/etc", Destination: router.IndexMount, RW: false}}
			},
			"index: mounts are not exactly the read-only index directory",
		},
		{
			"the index publishing a port",
			func(_, index *router.ContainerInspect) {
				index.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}}}
			},
			"index: publishes a port",
		},
		{
			"the index routed elsewhere",
			func(_, index *router.ContainerInspect) {
				index.Config.Labels["traefik.http.routers.berth-index.rule"] = "Host(`evil.localhost`)"
			},
			"index: routing labels differ",
		},
		{
			"a router carrying a lower protocol label",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.Config.Labels = testLabels("router")
				routerDoc.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol - 1)
			},
			"router: protocol label",
		},
		{
			"a writable root filesystem",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.ReadonlyRootfs = false },
			"router: root filesystem is writable",
		},
		{
			"kept capabilities",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.CapDrop = nil },
			"router: capabilities are not dropped",
		},
		{
			"extra capabilities",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.HostConfig.CapAdd = []string{"CAP_NET_BIND_SERVICE", "CAP_SYS_ADMIN"}
			},
			"router: capabilities beyond NET_BIND_SERVICE",
		},
		{
			"a port on every interface",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "80"}}}
			},
			"published only on 127.0.0.1:80",
		},
		{
			"an extra published port",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.NetworkSettings.Ports["8080/tcp"] = []router.InspectPortBinding{{HostIP: "127.0.0.1", HostPort: "8080"}}
			},
			"unexpected ports (8080/tcp)",
		},
		{
			"an unlabelled foreign network",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.NetworkSettings.Networks["evil"] = json.RawMessage("{}")
			},
			"attached to networks",
		},
		{
			"the file provider removed",
			func(routerDoc, _ *router.ContainerInspect) {
				var filtered []string
				for _, arg := range router.RouterCommand() {
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
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.Config.Image = "traefik:latest" },
			"router: image is not the pinned digest",
		},
		{
			"an extra mount",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.Mounts = append(routerDoc.Mounts, router.InspectMount{Type: "bind", Source: "/etc", Destination: "/host-etc", RW: false})
			},
			"router: mounts are not exactly",
		},
		{
			"a writable dynamic directory",
			func(routerDoc, _ *router.ContainerInspect) {
				for i := range routerDoc.Mounts {
					routerDoc.Mounts[i].RW = true
				}
			},
			"router: mounts are not exactly",
		},
		{
			"an unhealthy router",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.State = router.InspectState{Status: "running", Health: router.InspectHealth{Status: "unhealthy"}}
			},
			"router: not healthy",
		},
		{
			"a foreign container",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.Config.Labels = testLabels("router")
				routerDoc.Config.Labels["com.docker.compose.project"] = "other"
			},
			"router: not the container Compose started for this project",
		},
		{
			"a privileged router",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.Privileged = true },
			"router: privileged",
		},
		{
			"the router sharing the host PID namespace",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.PidMode = "host" },
			"router: shares a host namespace",
		},
		{
			"the router sharing the host IPC namespace",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.IpcMode = "host" },
			"router: shares a host namespace",
		},
		{
			"the router in host network mode",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.NetworkMode = "host" },
			"router: shares a host namespace",
		},
		{
			"the router sharing the host UTS namespace",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.HostConfig.UTSMode = "host" },
			"router: shares a host namespace",
		},
		{
			"a device attached to the router",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.HostConfig.Devices = []router.InspectDevice{{PathOnHost: "/dev/net/tun"}}
			},
			"router: devices are attached",
		},
		{
			"unconfined seccomp on the router",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.HostConfig.SecurityOpt = append(routerDoc.HostConfig.SecurityOpt, "seccomp=unconfined")
			},
			"router: seccomp is unconfined",
		},
		{
			"unconfined AppArmor on the router",
			func(routerDoc, _ *router.ContainerInspect) {
				routerDoc.HostConfig.SecurityOpt = append(routerDoc.HostConfig.SecurityOpt, "apparmor=unconfined")
			},
			"router: apparmor is unconfined",
		},
		{
			"a router entrypoint that differs from the pinned image",
			func(routerDoc, _ *router.ContainerInspect) { routerDoc.Config.Entrypoint = []string{"/bin/sh"} },
			"router: entrypoint differs from the pinned image",
		},
		{
			"an index entrypoint that differs from the pinned image",
			func(_, indexDoc *router.ContainerInspect) { indexDoc.Config.Entrypoint = []string{"/bin/sh"} },
			"index: entrypoint differs from the pinned image",
		},
	}

	for _, tc := range tampering {
		t.Run("reports "+tc.name, func(t *testing.T) {
			w := newWorld(t, 80, "")
			routerDoc := routerDoc(w.State.DynamicDir, w.Port)
			indexDoc := indexDoc(w.State.IndexDir)
			tc.tamper(&routerDoc, &indexDoc)
			w.Fake.setDoc("router-1", routerDoc)
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

	// Found in review, round 2 of #55: the network-membership check used to
	// compare attached networks against the expected set as an exact match,
	// so a labelled worktree network that exists but is not attached yet —
	// the window before this router's own next Ensure calls
	// ConnectNetwork, or during a worktree's own teardown before the
	// network is actually gone — read as drift, which could make Ensure
	// force-recreate a perfectly healthy router and made status/doctor fail
	// over a race that was never a problem in the first place. These two
	// pin both directions: an unattached labelled network is not reported,
	// and a router genuinely missing from its own shared network still is.
	t.Run("does not report a labelled worktree network the router is not attached to yet", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.networkLs = "berth-a1b2c3d4-router"
		w.Fake.mu.Unlock()

		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 0 {
			t.Errorf("problems = %v, want none: an unattached labelled network is not a stack problem", problems)
		}
	})

	t.Run("still reports the router when it is not attached to its own shared network", func(t *testing.T) {
		w := newWorld(t, 80, "")
		routerDoc := routerDoc(w.State.DynamicDir, w.Port)
		routerDoc.NetworkSettings.Networks = map[string]json.RawMessage{"evil": json.RawMessage("{}")}
		w.Fake.setDoc("router-1", routerDoc)

		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "attached to networks") {
			t.Fatalf("problems = %v, want exactly one naming the network mismatch", problems)
		}
	})

	// #55, found in review: routerProblems' own network-membership check
	// needs LabelledWorktreeNetworks to succeed to compare against anything
	// at all; a Docker failure there is reported as one problem of its own,
	// not silently ignored or turned into a Verify error outright (which
	// would end the command rather than let a caller like Ensure decide
	// what to do about a router that could not be fully checked).
	t.Run("reports a problem, rather than failing outright, when it cannot list this router's own worktree networks", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.networkLsFails = true
		w.Fake.mu.Unlock()

		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "could not list this router's worktree networks") {
			t.Fatalf("problems = %v", problems)
		}
	})
}
