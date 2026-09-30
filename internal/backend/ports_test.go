package backend

// Pinning tests for the loopback ports #119 publishes for routed services.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

func TestPublishedLoopbackPorts(t *testing.T) {
	ctx := context.Background()

	t.Run("publishes a routed service on 127.0.0.1 at its pinned port, targeting the routed container port", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "api", HostPort: 30001}, {Service: "worker", Port: intPtr(9091), HostPort: 30002}})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		services := overrideDoc(t, controlHome)["services"].(map[string]any)
		want := func(target float64, published string) []any {
			return []any{map[string]any{"target": target, "published": published, "host_ip": "127.0.0.1", "protocol": "tcp"}}
		}
		if got := services["api"].(map[string]any)["ports"]; !reflectEqual(got, want(8080, "30001")) {
			t.Fatalf("api ports = %v", got)
		}
		if got := services["worker"].(map[string]any)["ports"]; !reflectEqual(got, want(9091, "30002")) {
			t.Fatalf("worker ports = %v (the explicit routed port is the target)", got)
		}
		if _, published := services["quiet"].(map[string]any)["ports"]; published {
			t.Fatal("an unrouted service must publish nothing")
		}
	})

	t.Run("publishes nothing for a route with no pinned port", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "api"}})
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		if _, published := overrideDoc(t, controlHome)["services"].(map[string]any)["api"].(map[string]any)["ports"]; published {
			t.Fatal("the override published a port nobody allocated")
		}
	})

	t.Run("writes identical override bytes on a second up, so Compose does not recreate containers", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "api", HostPort: 30001}, {Service: "worker", HostPort: 30002}})
		ensureHome(t, controlHome)
		var written [][]byte
		for range 2 {
			if err := rt.Up(ctx, ref, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(overridePathFor(controlHome))
			if err != nil {
				t.Fatal(err)
			}
			written = append(written, data)
		}
		if !bytes.Equal(written[0], written[1]) {
			t.Fatalf("override changed between ups:\n%s\n---\n%s", written[0], written[1])
		}
		if !bytes.Contains(written[0], []byte(`"published": "30001"`)) {
			t.Fatalf("override does not publish the port:\n%s", written[0])
		}
	})

	t.Run("a different port changes the override, as it must", func(t *testing.T) {
		render := func(port int) []byte {
			fake := newFake()
			rt, controlHome := routedRuntime(t, fake, []RouteSpec{{Service: "api", HostPort: port}})
			ensureHome(t, controlHome)
			if err := rt.Up(ctx, ref, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(overridePathFor(controlHome))
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		if bytes.Equal(render(30001), render(30002)) {
			t.Fatal("two ports rendered the same override")
		}
	})
}

func TestRefusesUnpublishableRoutedServices(t *testing.T) {
	route := func(t *testing.T, model string) error {
		t.Helper()
		services, volumes, networks := decodeModel(t, model)
		rt := composeWithRoute("web", nil)
		_, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks)
		return err
	}
	for name, tc := range map[string]struct{ model, want string }{
		"network_mode: host": {
			`{"services":{"web":{"expose":["80"],"network_mode":"host"}}}`,
			"Service web in /work/acme/docker/compose.yaml uses network_mode: host",
		},
		"network_mode: service:db": {
			`{"services":{"web":{"expose":["80"],"network_mode":"service:db"}}}`,
			"Service web in /work/acme/docker/compose.yaml uses network_mode: service:db",
		},
		"deploy.replicas above one": {
			`{"services":{"web":{"expose":["80"],"deploy":{"replicas":3}}}}`,
			"Service web in /work/acme/docker/compose.yaml runs 3 replicas",
		},
		"scale above one": {
			`{"services":{"web":{"expose":["80"],"scale":2}}}`,
			"Service web in /work/acme/docker/compose.yaml runs 2 replicas",
		},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			err := route(t, tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	for name, model := range map[string]string{
		"a bridge network_mode":  `{"services":{"web":{"expose":["80"],"network_mode":"bridge"}}}`,
		"one replica":            `{"services":{"web":{"expose":["80"],"deploy":{"replicas":1},"scale":1}}}`,
		"deploy without replica": `{"services":{"web":{"expose":["80"],"deploy":{"resources":{}}}}}`,
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			if err := route(t, model); err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}

	t.Run("does not refuse an unrouted service that uses the host network", func(t *testing.T) {
		services, volumes, networks := decodeModel(t, `{"services":{"web":{"expose":["80"]},"agent":{"network_mode":"host"}}}`)
		rt := composeWithRoute("web", nil)
		if _, err := rt.overrideDocument(networkFixtureRef, services, volumes, networks); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPortPublishers(t *testing.T) {
	ctx := context.Background()

	t.Run("classifies each container publishing the port by its ownership labels, not its name", func(t *testing.T) {
		fake := newFake()
		fake.resources["ps"] = []string{"own1", "foreign1", "lookalike1"}
		fake.own("own1", "api", true)
		fake.labels["own1"]["name"] = "acme-api-1"
		// Another project's berth container: managed, but a different worktree.
		fake.labels["foreign1"] = map[string]string{
			ns + ".managed": "true", ns + ".repository-uuid": ref.RepositoryID, ns + ".worktree-uuid": "99999999-9999-4999-8999-999999999999",
			"name": "other-api-1",
		}
		// Named like ours, carrying none of our labels.
		fake.labels["lookalike1"] = map[string]string{"name": "acme-api-1"}
		rt, _ := newRuntime(t, fake)

		publishers, err := rt.PortPublishers(ctx, ref, 30001)
		if err != nil {
			t.Fatal(err)
		}
		if len(publishers) != 3 {
			t.Fatalf("publishers = %+v", publishers)
		}
		own := map[string]bool{}
		for _, p := range publishers {
			own[p.ID] = p.Own
		}
		if !own["own1"] || own["foreign1"] || own["lookalike1"] {
			t.Fatalf("ownership = %v", own)
		}
		if publishers[0].Name != "acme-api-1" || publishers[0].Service != "api" {
			t.Fatalf("own container = %+v", publishers[0])
		}
		var listed []string
		for _, call := range fake.calls {
			if len(call.Args) > 0 && call.Args[0] == "ps" {
				listed = call.Args
			}
		}
		if strings.Join(listed, " ") != "ps --filter publish=30001 --format {{.ID}}" {
			t.Fatalf("listing = %v", listed)
		}
	})

	t.Run("leaves out a container that vanished before it was inspected", func(t *testing.T) {
		fake := newFake()
		fake.resources["ps"] = []string{"gone1"}
		fake.uninspected["gone1"] = true
		rt, _ := newRuntime(t, fake)
		publishers, err := rt.PortPublishers(ctx, ref, 30001)
		if err != nil || len(publishers) != 0 {
			t.Fatalf("publishers = %+v, %v", publishers, err)
		}
	})

	t.Run("finds nothing when nothing publishes the port", func(t *testing.T) {
		rt, _ := newRuntime(t, newFake())
		publishers, err := rt.PortPublishers(ctx, ref, 30001)
		if err != nil || len(publishers) != 0 {
			t.Fatalf("publishers = %+v, %v", publishers, err)
		}
	})
}

// configRunner answers `docker compose config` with a fixed document.
type configRunner struct{ document string }

// Run is run.Runner's Run.
func (r configRunner) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	return run.Finish(spec, run.Result{Stdout: r.document})
}

// Which is run.Runner's Which.
func (configRunner) Which(string) bool { return true }

func TestFixedPublishedPorts(t *testing.T) {
	document := `{"services":{
		"api":{"ports":[{"mode":"ingress","target":80,"published":"8080","protocol":"tcp"},{"target":81,"protocol":"tcp"}]},
		"db":{"ports":[{"mode":"ingress","host_ip":"127.0.0.1","target":5432,"published":"5432","protocol":"tcp"}]},
		"web":{"expose":["3000"]},
		"zero":{"ports":[{"target":9,"published":"0"}]}}}`
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/compose.yaml", ProjectRoot: "/work/acme", ControlHome: t.TempDir(), LabelNamespace: ns,
		Runner: configRunner{document}, Logger: logger.Silent{},
	})
	got, err := rt.FixedPublishedPorts(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]FixedPort{"api": {{Published: "8080", Target: 80}}, "db": {{Published: "5432", Target: 5432}}}
	if !reflectEqual(got, want) {
		t.Fatalf("FixedPublishedPorts = %+v, want %+v", got, want)
	}
}

func TestServicePublishers(t *testing.T) {
	fake := newFake()
	fake.resources["ps"] = []string{"own1"}
	fake.own("own1", "api", true)
	fake.labels["own1"]["name"] = "acme-api-1"
	fake.labels["own1"]["ports"] = `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"30001"}],"9/tcp":[{"HostIp":"0.0.0.0","HostPort":"9"}],"7/tcp":null}`
	rt, _ := newRuntime(t, fake)

	publishers, err := rt.ServicePublishers(context.Background(), ref, "api")
	if err != nil || len(publishers) != 1 {
		t.Fatalf("publishers = %+v, %v", publishers, err)
	}
	if !reflectEqual(publishers[0].LoopbackPorts, []int{30001}) || !publishers[0].Own {
		t.Fatalf("publisher = %+v", publishers[0])
	}
	var listed string
	for _, call := range fake.calls {
		if len(call.Args) > 0 && call.Args[0] == "ps" {
			listed = strings.Join(call.Args, " ")
		}
	}
	for _, want := range []string{"label=" + ns + ".worktree-uuid=" + ref.WorktreeID, "label=" + ComposeServiceLabel + "=api"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("listing %q lacks %q", listed, want)
		}
	}
}

func TestCheckPublishable(t *testing.T) {
	check := func(document string) error {
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/compose.yaml", ProjectRoot: "/work/acme", ControlHome: t.TempDir(), LabelNamespace: ns,
			Routes: []RouteSpec{{Service: "web"}}, Runner: configRunner{document}, Logger: logger.Silent{},
		})
		return rt.CheckPublishable(context.Background(), ref)
	}
	if err := check(`{"services":{"web":{"expose":["80"],"network_mode":"host"}}}`); err == nil || !strings.Contains(err.Error(), "network_mode: host") {
		t.Fatalf("err = %v", err)
	}
	if err := check(`{"services":{"web":{"expose":["80"],"scale":3}}}`); err == nil || !strings.Contains(err.Error(), "3 replicas") {
		t.Fatalf("err = %v", err)
	}
	if err := check(`{"services":{"web":{"expose":["80"]}}}`); err != nil {
		t.Fatalf("err = %v", err)
	}
	if err := check(`{"services":{}}`); err != nil {
		t.Fatalf("a service the file lacks is prepare's to report: %v", err)
	}
}

// failingConfigRunner answers `docker compose config` with a Compose error.
type failingConfigRunner struct{}

// Run is run.Runner's Run.
func (failingConfigRunner) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	return run.Finish(spec, run.Result{ExitCode: 1, Stderr: "services.web.image must be a string"})
}

// Which is run.Runner's Which.
func (failingConfigRunner) Which(string) bool { return true }

func TestCheckPublishableWhenComposeCannotLoad(t *testing.T) {
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/compose.yaml", ProjectRoot: "/work/acme", ControlHome: t.TempDir(), LabelNamespace: ns,
		Routes: []RouteSpec{{Service: "web"}}, Runner: failingConfigRunner{}, Logger: logger.Silent{},
	})
	err := rt.CheckPublishable(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "Compose could not read /work/acme/compose.yaml") ||
		!strings.Contains(err.Error(), "services.web.image must be a string") {
		t.Fatalf("err = %v: a Compose file that fails to load is Compose's own error, reported before any port is allocated", err)
	}
	// With nothing routed the file is not read at all.
	unrouted := NewCompose(Options{ComposeFile: "/work/acme/compose.yaml", Runner: failingConfigRunner{}, Logger: logger.Silent{}})
	if err := unrouted.CheckPublishable(context.Background(), ref); err != nil {
		t.Fatalf("no routes: %v", err)
	}
}
