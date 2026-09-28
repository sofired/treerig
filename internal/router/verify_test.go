package router_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
		{
			"an index started without an init process",
			func(_, indexDoc *router.ContainerInspect) { indexDoc.HostConfig.Init = nil },
			"index: runs without an init process, so stopping it waits out Docker's grace period",
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

	// Every problem also has a code (Problem.Code), the fixed vocabulary the
	// JSON forms key on: "<service>.<what>", the service matching the
	// message's own prefix, and unique within one status. Each tampering
	// yields exactly one problem, whose code must be documented.
	t.Run("gives every tampering its own documented code under its own service", func(t *testing.T) {
		// The code each tampering's expected message must carry.
		codeByExpected := map[string]string{
			"index: mounts are not exactly the read-only index directory": "index.mounts",
			"index: publishes a port":                                     "index.port",
			"index: routing labels differ":                                "index.labels",
			"router: protocol label":                                      "router.protocol-label",
			"router: root filesystem is writable":                         "router.writable-rootfs",
			"router: capabilities are not dropped":                        "router.capabilities-kept",
			"router: capabilities beyond NET_BIND_SERVICE":                "router.capabilities",
			"published only on 127.0.0.1:80":                              "router.port",
			"unexpected ports (8080/tcp)":                                 "router.other-ports",
			"attached to networks":                                        "router.networks",
			"router: command line differs":                                "router.command",
			"router: image is not the pinned digest":                      "router.image",
			"router: mounts are not exactly":                              "router.mounts",
			"router: not healthy":                                         "router.not-healthy",
			"router: not the container Compose started for this project":  "router.container",
			"router: privileged":                                          "router.privileged",
			"router: shares a host namespace":                             "router.host-namespace",
			"router: devices are attached":                                "router.devices",
			"router: seccomp is unconfined":                               "router.seccomp",
			"router: apparmor is unconfined":                              "router.apparmor",
			"router: entrypoint differs from the pinned image":            "router.entrypoint",
			"index: entrypoint differs from the pinned image":             "index.entrypoint",
			"index: runs without an init process, so stopping it waits out Docker's grace period": "index.init",
		}
		seen := map[string]bool{}
		for _, tc := range tampering {
			w := newWorld(t, 80, "")
			routerD := routerDoc(w.State.DynamicDir, w.Port)
			indexD := indexDoc(w.State.IndexDir)
			tc.tamper(&routerD, &indexD)
			w.Fake.setDoc("router-1", routerD)
			w.Fake.setDoc("index-1", indexD)
			status, err := w.Router.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(status.Problems) != 1 {
				t.Fatalf("%s: problems = %v, want exactly one", tc.name, status.Problems)
			}
			problem := status.Problems[0]
			service, _, _ := strings.Cut(problem.Message, ":")
			if !strings.HasPrefix(problem.Code, service+".") {
				t.Errorf("%s: code %q does not belong to the service of %q", tc.name, problem.Code, problem.Message)
			}
			if !slices.Contains(documentedRouterCodes, problem.Code) {
				t.Errorf("%s: code %q is not in documentedRouterCodes", tc.name, problem.Code)
			}
			if want, ok := codeByExpected[tc.expected]; !ok || problem.Code != want {
				t.Errorf("%s: code = %q, want %q", tc.name, problem.Code, want)
			}
			seen[problem.Code] = true
		}
		// The codes the table does not reach are pinned by their own tests
		// below.
		if len(seen) != len(codeByExpected) {
			t.Errorf("the tamperings gave %d distinct codes, want %d", len(seen), len(codeByExpected))
		}
	})

	t.Run("reports each code once when the tamperings are all applied at once", func(t *testing.T) {
		w := newWorld(t, 80, "")
		routerD := routerDoc(w.State.DynamicDir, w.Port)
		indexD := indexDoc(w.State.IndexDir)
		for _, tc := range tampering {
			tc.tamper(&routerD, &indexD)
		}
		w.Fake.setDoc("router-1", routerD)
		w.Fake.setDoc("index-1", indexD)
		status, err := w.Router.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, problem := range status.Problems {
			if seen[problem.Code] {
				t.Errorf("code %q appears twice", problem.Code)
			}
			seen[problem.Code] = true
		}
	})

	t.Run("reports a duplicated unconfined security option once", func(t *testing.T) {
		w := newWorld(t, 80, "")
		routerD := routerDoc(w.State.DynamicDir, w.Port)
		routerD.HostConfig.SecurityOpt = append(routerD.HostConfig.SecurityOpt,
			"seccomp=unconfined", "seccomp=unconfined", "apparmor=unconfined", "apparmor=unconfined")
		w.Fake.setDoc("router-1", routerD)
		w.Fake.setDoc("index-1", indexDoc(w.State.IndexDir))

		problems, err := w.Router.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"router: seccomp is unconfined", "router: apparmor is unconfined"}
		if !slices.Equal(problems, want) {
			t.Fatalf("problems = %v, want %v", problems, want)
		}
	})

	// The codes for what is gone or unreachable, rather than tampered with,
	// are pinned with the vocabulary in documentedRouterCodes.
	t.Run("codes a router that is gone, half gone and unreachable from the same vocabulary", func(t *testing.T) {
		codeOf := func(w *testWorld) string {
			status, err := w.Router.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(status.Problems) != 1 {
				t.Fatalf("problems = %v", status.Problems)
			}
			return status.Problems[0].Code
		}
		gone := newWorld(t, 80, "")
		gone.Fake.mu.Lock()
		gone.Fake.ids = map[string]string{}
		gone.Fake.mu.Unlock()
		half := newWorld(t, 80, "")
		half.Fake.mu.Lock()
		half.Fake.ids = map[string]string{"router": "router-1"}
		half.Fake.mu.Unlock()
		down := newWorld(t, 80, "")
		down.Fake.psExitCode = 1
		down.Fake.psStderr = "failed to connect to the docker API at unix:///var/run/docker.sock\n"
		for want, w := range map[string]*testWorld{"router.absent": gone, "index.missing": half, "stack.unreachable": down} {
			if got := codeOf(w); got != want {
				t.Errorf("code = %q, want %q", got, want)
			}
			if !slices.Contains(documentedRouterCodes, want) {
				t.Errorf("%q is not in documentedRouterCodes", want)
			}
		}
	})

	t.Run("documents every code in the README", func(t *testing.T) {
		readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, code := range documentedRouterCodes {
			if !strings.Contains(string(readme), "`"+code+"`") {
				t.Errorf("README.md does not list the router code %q", code)
			}
		}
	})

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

// documentedRouterCodes is every Problem.Code a router status can carry; the
// README lists them. A new check must be added here and there.
var documentedRouterCodes = []string{
	"router.container", "router.component-label", "router.protocol-label", "router.not-running", "router.not-healthy",
	"router.writable-rootfs", "router.new-privileges", "router.capabilities-kept", "router.privileged",
	"router.host-namespace", "router.devices", "router.seccomp", "router.apparmor",
	"router.port", "router.other-ports", "router.network-list", "router.networks", "router.command",
	"router.image", "router.entrypoint", "router.capabilities", "router.mounts",
	"index.container", "index.component-label", "index.protocol-label", "index.not-running", "index.not-healthy",
	"index.writable-rootfs", "index.new-privileges", "index.capabilities-kept", "index.privileged",
	"index.host-namespace", "index.devices", "index.seccomp", "index.apparmor",
	"index.image", "index.entrypoint", "index.command", "index.labels", "index.network", "index.port",
	"index.capabilities", "index.init", "index.mounts",
	"router.missing", "index.missing", "router.absent", "stack.inspect", "stack.unreachable",
}
