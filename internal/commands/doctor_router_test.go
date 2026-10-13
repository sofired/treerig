package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
)

// Identifiers shaped like the ones the registry writes, which its strict
// reader requires of a record's file name.
const (
	routeRepo     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	routeWorktree = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// routeClientFunc adapts a function to routeClient.
type routeClientFunc func(request *http.Request) (*http.Response, error)

// Do calls f.
func (f routeClientFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

// plainResponse is an HTTP response with a status, content type and body.
func plainResponse(status int, contentType, body string) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

// traefikNotFound is the response Traefik gives for a host no route matches.
func traefikNotFound() *http.Response {
	return plainResponse(http.StatusNotFound, "text/plain; charset=utf-8", "404 page not found\n")
}

// routerStateWithRoutes is a router layout whose dynamic directory holds the
// index route and one route file per (service, rule) pair given.
func routerStateWithRoutes(t *testing.T, rules map[string]string) layout.RouterLayout {
	t.Helper()
	state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
	if err := router.WriteIndexFragment(state, 30500); err != nil {
		t.Fatal(err)
	}
	for service, rule := range rules {
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, service,
			router.NewFragmentDocument("berth-aaa-"+service, rule, 30501)); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// portClient routes a request to the handler registered for its loopback
// port, counting the requests each port saw and recording the last one.
type portClient struct {
	mu       sync.Mutex
	handlers map[int]func(*http.Request) (*http.Response, error)
	counts   map[int]int
	last     map[int]*http.Request
}

// newPortClient is a portClient with the given handlers.
func newPortClient(handlers map[int]func(*http.Request) (*http.Response, error)) *portClient {
	return &portClient{handlers: handlers, counts: map[int]int{}, last: map[int]*http.Request{}}
}

// Do dispatches on the request's port; a port with no handler refuses.
func (c *portClient) Do(request *http.Request) (*http.Response, error) {
	_, portText, _ := net.SplitHostPort(request.URL.Host)
	port, _ := strconv.Atoi(portText)
	c.mu.Lock()
	c.counts[port]++
	c.last[port] = request
	handler := c.handlers[port]
	c.mu.Unlock()
	if handler == nil {
		return nil, errors.New("connection refused")
	}
	return handler(request)
}

// count is how many requests port has seen.
func (c *portClient) count(port int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[port]
}

// reply is a handler answering a fixed status.
func reply(status int) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) { return plainResponse(status, "text/plain", "x"), nil }
}

func TestProbeRoute(t *testing.T) {
	const routerPort, targetPort = 1, 2
	target := routeTarget{claim: "web.main.acme.localhost/v1", host: "web.main.acme.localhost", path: "/v1", targetPort: targetPort}
	run := func(client routeClient, budget time.Duration) routeProbeResult {
		return probeRoute(context.Background(), client, routerPort, target, budget, time.Millisecond)
	}

	t.Run("retries until a route that was not loaded yet answers", func(t *testing.T) {
		var calls atomic.Int32
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){
			routerPort: func(*http.Request) (*http.Response, error) {
				if calls.Add(1) <= 2 {
					return traefikNotFound(), nil
				}
				return plainResponse(200, "text/plain", "ok"), nil
			},
			targetPort: reply(200),
		})
		result := run(client, time.Second)
		if result.answer != answerOK || result.status != 200 || calls.Load() != 3 {
			t.Fatalf("result = %+v after %d calls", result, calls.Load())
		}
	})

	t.Run("a 404 the target gives for the same request is the service's own", func(t *testing.T) {
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){
			routerPort: reply(404), targetPort: reply(404),
		})
		result := run(client, time.Second)
		if result.answer != answerServiceNotFound || result.status != 404 || client.count(routerPort) != 1 {
			t.Fatalf("result = %+v after %d router requests", result, client.count(routerPort))
		}
	})

	t.Run("a 404 the target does not reproduce is not loaded, retried to the end of the budget", func(t *testing.T) {
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){
			routerPort: traefikNotFoundHandler, targetPort: reply(200),
		})
		result := run(client, 30*time.Millisecond)
		if result.answer != answerNotLoaded || client.count(routerPort) < 3 {
			t.Fatalf("result = %+v after %d router requests", result, client.count(routerPort))
		}
	})

	t.Run("a 404 with a target that does not answer is a target down, retried at most maxDownRetries times", func(t *testing.T) {
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){routerPort: traefikNotFoundHandler})
		result := run(client, time.Minute)
		if result.answer != answerTargetDown || client.count(routerPort) != 1+maxDownRetries {
			t.Fatalf("result = %+v after %d router requests", result, client.count(routerPort))
		}
	})

	for _, status := range []int{502, 503, 504} {
		t.Run("a "+strconv.Itoa(status)+" is a target down, retried at most maxDownRetries times", func(t *testing.T) {
			client := newPortClient(map[int]func(*http.Request) (*http.Response, error){routerPort: reply(status)})
			result := run(client, time.Minute)
			if result.answer != answerTargetDown || result.status != status || client.count(routerPort) != 1+maxDownRetries {
				t.Fatalf("result = %+v after %d router requests", result, client.count(routerPort))
			}
			if client.count(targetPort) != 0 {
				t.Fatalf("asked the target %d times for a %d", client.count(targetPort), status)
			}
		})
	}

	t.Run("a router that does not answer is retried at most maxDownRetries times", func(t *testing.T) {
		client := newPortClient(nil)
		result := run(client, time.Minute)
		if result.answer != answerNoProxy || result.err == nil || client.count(routerPort) != 1+maxDownRetries {
			t.Fatalf("result = %+v after %d router requests", result, client.count(routerPort))
		}
	})

	t.Run("a 200 answers at once", func(t *testing.T) {
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){routerPort: reply(200)})
		result := run(client, time.Second)
		if result.answer != answerOK || result.status != 200 || client.count(routerPort) != 1 {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("sends the direct request with the same path and Host to the target's loopback port", func(t *testing.T) {
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){
			routerPort: reply(404), targetPort: reply(404),
		})
		run(client, time.Second)
		direct := client.last[targetPort]
		if direct == nil || direct.URL.Host != "127.0.0.1:2" || direct.URL.Path != "/v1" || direct.Host != "web.main.acme.localhost" {
			t.Fatalf("direct request = %+v", direct)
		}
	})

	t.Run("stops retrying when the context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		client := newPortClient(map[int]func(*http.Request) (*http.Response, error){
			routerPort: func(*http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					cancel()
				}
				return traefikNotFound(), nil
			},
			targetPort: reply(200),
		})
		started := time.Now()
		result := probeRoute(ctx, client, routerPort, target, time.Minute, time.Minute)
		if result.answer != answerNotLoaded || calls.Load() != 1 || time.Since(started) > 5*time.Second {
			t.Fatalf("result = %+v after %d calls", result, calls.Load())
		}
	})

	t.Run("tells a router stub from its target stub over real sockets", func(t *testing.T) {
		targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1" {
				_, _ = io.WriteString(w, "hello")
				return
			}
			http.NotFound(w, r)
		}))
		defer targetServer.Close()
		realTarget := targetServer.Listener.Addr().(*net.TCPAddr).Port
		routerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A router that loads only the api route.
			if r.Host != "api.main.acme.localhost" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "404 page not found\n")
				return
			}
			forward, err := http.NewRequest(http.MethodGet, targetServer.URL+r.URL.Path, nil)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			forward.Host = r.Host
			response, err := http.DefaultClient.Do(forward)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer func() { _ = response.Body.Close() }()
			w.WriteHeader(response.StatusCode)
		}))
		defer routerServer.Close()
		realRouter := routerServer.Listener.Addr().(*net.TCPAddr).Port

		routed := routeTarget{claim: "api.main.acme.localhost/v1", host: "api.main.acme.localhost", path: "/v1", targetPort: realTarget}
		if got := probeRoute(context.Background(), routerServer.Client(), realRouter, routed, time.Second, time.Millisecond); got.answer != answerOK {
			t.Fatalf("a loaded route: result = %+v", got)
		}
		missing := routeTarget{claim: "api.main.acme.localhost/other", host: "api.main.acme.localhost", path: "/other", targetPort: realTarget}
		if got := probeRoute(context.Background(), routerServer.Client(), realRouter, missing, 20*time.Millisecond, 5*time.Millisecond); got.answer != answerServiceNotFound {
			t.Fatalf("the service's own 404: result = %+v", got)
		}
		unrouted := routeTarget{claim: "nobody.localhost", host: "nobody.localhost", path: "/v1", targetPort: realTarget}
		if got := probeRoute(context.Background(), routerServer.Client(), realRouter, unrouted, 20*time.Millisecond, 5*time.Millisecond); got.answer != answerNotLoaded {
			t.Fatalf("an unloaded route: result = %+v", got)
		}
	})
}

// traefikNotFoundHandler answers the router's own 404.
func traefikNotFoundHandler(*http.Request) (*http.Response, error) { return traefikNotFound(), nil }

func TestRouteProbeCheck(t *testing.T) {
	target := routeTarget{claim: "web.main.acme.localhost"}
	t.Run("an answering route is ok", func(t *testing.T) {
		got := routeProbeCheck(target, 80, routeProbeResult{answer: answerOK, status: 200})
		want := Check{ID: "router.route-probe", Instance: "web.main.acme.localhost", Status: StatusOK, Subject: "routes", Detail: "web.main.acme.localhost answers (HTTP 200)"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("a 404 the service gives itself is ok", func(t *testing.T) {
		got := routeProbeCheck(target, 80, routeProbeResult{answer: answerServiceNotFound, status: 404})
		if got.Status != StatusOK || !strings.Contains(got.Detail, "the service itself answers HTTP 404") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a route that is not loaded fails", func(t *testing.T) {
		got := routeProbeCheck(target, 80, routeProbeResult{answer: answerNotLoaded, status: 404, elapsed: 8 * time.Second})
		if got.Status != StatusFail || !strings.Contains(got.Detail, "route is not loaded") || !strings.Contains(got.Detail, "8 s") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a target that is down warns", func(t *testing.T) {
		for _, status := range []int{502, 503, 504} {
			got := routeProbeCheck(routeTarget{claim: "web.main.acme.localhost", targetPort: 30501}, 80, routeProbeResult{answer: answerTargetDown, status: status, elapsed: time.Second})
			if got.Status != StatusWarn || !strings.Contains(got.Detail, fmt.Sprintf("the router answers %d", status)) || !strings.Contains(got.Detail, "down or unreachable") {
				t.Fatalf("status %d: got %+v", status, got)
			}
		}
	})
	t.Run("a router 404 with an unreachable target says the target is down and the route may not be loaded", func(t *testing.T) {
		got := routeProbeCheck(routeTarget{claim: "web.main.acme.localhost", targetPort: 30501}, 80, routeProbeResult{answer: answerTargetDown, status: 404})
		want := "web.main.acme.localhost: the router answers 404 for it and 127.0.0.1:30501 does not answer, so the target is down; if it is up and this persists, the route may not be loaded"
		if got.Status != StatusWarn || got.Detail != want {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a router that does not answer fails, naming the port and the error", func(t *testing.T) {
		got := routeProbeCheck(target, 8080, routeProbeResult{answer: answerNoProxy, err: errors.New("connection refused")})
		if got.Status != StatusFail || !strings.Contains(got.Detail, "127.0.0.1:8080") || !strings.Contains(got.Detail, "connection refused") {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRouteTargets(t *testing.T) {
	t.Run("lists the index and every route file by claim, with host, path and target port, skipping rules berth did not write", func(t *testing.T) {
		state := routerStateWithRoutes(t, map[string]string{
			"web": "Host(`web.main.acme.localhost`)",
			"api": "Host(`main.acme.localhost`) && PathPrefix(`/api`)",
			"odd": "HostRegexp(`.+`)",
		})
		got, err := routeTargets(state)
		if err != nil {
			t.Fatal(err)
		}
		want := []routeTarget{
			{claim: "berth.localhost", host: "berth.localhost", path: "/", targetPort: 30500},
			{claim: "main.acme.localhost/api", host: "main.acme.localhost", path: "/api", targetPort: 30501},
			{claim: "web.main.acme.localhost", host: "web.main.acme.localhost", path: "/", targetPort: 30501},
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		}
	})
	t.Run("lists a host route's file too, with the port it targets", func(t *testing.T) {
		const repo, wt = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222221"
		state := layout.RouterStateFrom(registrytest.TempDir(t))
		if err := router.WriteIndexFragment(state, 30500); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "web", router.NewFragmentDocument("berth-aaa-web", "Host(`web.main.acme.localhost`)", 30501)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteHostFragment(state, repo, wt, "dev", router.HostFragmentDocument(repo, wt, "dev", "Host(`app.main.acme.localhost`)", 5173)); err != nil {
			t.Fatal(err)
		}
		got, err := routeTargets(state)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, target := range got {
			if target.claim == "app.main.acme.localhost" && target.targetPort == 5173 && target.path == "/" {
				found = true
			}
		}
		if !found || len(got) != 3 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("lists nothing for a directory that does not exist", func(t *testing.T) {
		got, err := routeTargets(layout.RouterLayout{DynamicDir: registrytest.TempDir(t) + "/missing"})
		if err != nil || len(got) != 0 {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestRouterModeCheck(t *testing.T) {
	check := func(fake *fakeRouterSource, status *router.RouterStatus) Check {
		t.Helper()
		got, err := routerModeCheck(context.Background(), doctorOptions{CommandName: "berth", Router: fake}, status)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	running := func(mode router.Mode) *router.RouterStatus {
		return &router.RouterStatus{Reachable: true, Running: true, Port: 80, RunningPort: 8080, Mode: mode}
	}

	t.Run("says what mode it would start in when nothing runs", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.mode = router.ModeBridge
		want := Check{ID: "router.mode", Status: StatusOK, Subject: "router mode", Detail: "not running; would start in bridge mode"}
		if got := check(fake, &router.RouterStatus{Reachable: true}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("is ok when the running mode is the one this build would use", func(t *testing.T) {
		want := Check{ID: "router.mode", Status: StatusOK, Subject: "router mode", Detail: "host mode, serving 127.0.0.1:8080"}
		if got := check(newFakeRouterSource(), running(router.ModeHost)); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("warns when the running mode differs, naming how it is fixed", func(t *testing.T) {
		got := check(newFakeRouterSource(), running(router.ModeBridge))
		if got.Status != StatusWarn || !strings.Contains(got.Detail, "running in bridge mode") ||
			!strings.Contains(got.Detail, "would use host mode") || !strings.Contains(got.Detail, "'berth up'") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("warns when the running router does not say its mode", func(t *testing.T) {
		got := check(newFakeRouterSource(), running(""))
		if got.Status != StatusWarn || !strings.Contains(got.Detail, "does not say which mode") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("does not claim the router is not running when Compose did not answer", func(t *testing.T) {
		got := check(newFakeRouterSource(), nil)
		if got.Status != StatusOK || strings.Contains(got.Detail, "not running") || !strings.Contains(got.Detail, "whether it runs is unknown") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("names an override, and warns when detection would pick another mode", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.mode, fake.override, fake.detected = router.ModeBridge, router.ModeBridge, router.ModeHost
		got := check(fake, &router.RouterStatus{Reachable: true})
		if got.Status != StatusWarn || !strings.Contains(got.Detail, "mode set by settings.toml") || !strings.Contains(got.Detail, "would pick host") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("names an override that agrees with detection, without a warning", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.override, fake.detected = router.ModeHost, router.ModeHost
		got := check(fake, running(router.ModeHost))
		if got.Status != StatusOK || !strings.Contains(got.Detail, "mode set by settings.toml") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("is ok with an override when detection alone would refuse", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.mode, fake.override, fake.detected = router.ModeBridge, router.ModeBridge, ""
		got := check(fake, &router.RouterStatus{Reachable: true})
		if got.Status != StatusOK || !strings.Contains(got.Detail, "mode set by settings.toml") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("reports a protocol mismatch with its advice and never says the next up recreates it", func(t *testing.T) {
		status := running(router.ModeHost)
		status.Problems = []router.Problem{{Code: "router.protocol", Message: "The running shared router has a different layout from this version's. Run 'berth router down' first, then run this again."}}
		got := check(newFakeRouterSource(), status)
		if got.Status != StatusWarn || !strings.Contains(got.Detail, "Run 'berth router down' first") || strings.Contains(got.Detail, "recreates") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("keeps a long refusal's advice instead of cutting it", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.modeErr = errors.New(strings.Repeat("Docker is somewhere unexpected. ", 14) + "Set [router] mode in settings.toml to bridge.")
		got := check(fake, nil)
		if got.Status != StatusFail || !strings.Contains(got.Detail, "Set [router] mode in settings.toml to bridge.") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("fails with the refusal's own text even when nothing runs", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.modeErr = errors.New("Rootless Docker is not supported yet (#121).")
		got := check(fake, &router.RouterStatus{Reachable: true})
		if got.Status != StatusFail || !strings.Contains(got.Detail, "Rootless Docker is not supported yet (#121).") {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRouterBindCheck(t *testing.T) {
	t.Run("is skipped without a probe", func(t *testing.T) {
		got, err := routerBindCheck(context.Background(), doctorOptions{}, 80)
		if got != nil || err != nil {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("is ok when no other address answers", func(t *testing.T) {
		opts := doctorOptions{LANProbe: func(context.Context, int) ([]string, error) { return nil, nil }}
		got, err := routerBindCheck(context.Background(), opts, 8080)
		want := Check{ID: "router.bind", Status: StatusOK, Subject: "router bind", Detail: "127.0.0.1:8080 only; no other local address answers"}
		if err != nil || got == nil || *got != want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("fails naming every address that answers", func(t *testing.T) {
		opts := doctorOptions{LANProbe: func(_ context.Context, port int) ([]string, error) {
			return []string{"192.168.1.5:80", "172.17.0.1:80"}, nil
		}}
		got, err := routerBindCheck(context.Background(), opts, 80)
		if err != nil || got == nil || got.Status != StatusFail ||
			!strings.Contains(got.Detail, "192.168.1.5:80") || !strings.Contains(got.Detail, "172.17.0.1:80") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("returns the cancellation, not a verdict, when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
		opts := doctorOptions{LANProbe: func(context.Context, int) ([]string, error) { return nil, nil }}
		got, err := routerBindCheck(ctx, opts, 80)
		var cancelled *clierr.Cancelled
		if got != nil || !errors.As(err, &cancelled) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("passes the probe's own error on", func(t *testing.T) {
		boom := errors.New("no interfaces")
		opts := doctorOptions{LANProbe: func(context.Context, int) ([]string, error) { return nil, boom }}
		if _, err := routerBindCheck(context.Background(), opts, 80); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLanAnsweringStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&clierr.Cancelled{Signal: syscall.SIGTERM})
	answering, err := lanAnswering(ctx, 1)
	var cancelled *clierr.Cancelled
	if answering != nil || !errors.As(err, &cancelled) {
		t.Fatalf("got %v, %v", answering, err)
	}
}

func TestRouterPortsCheck(t *testing.T) {
	conflict := router.PortConflict{Port: 80, What: "the router", Holders: "another process (find it with: ss)", Remedy: "choose another port"}
	absent := &router.RouterStatus{Reachable: true, Problems: []router.Problem{{Code: "router.absent"}}}
	unhealthy := &router.RouterStatus{Reachable: true, Running: true, Problems: []router.Problem{{Code: "router.not-healthy"}}}
	healthy := &router.RouterStatus{Reachable: true, Running: true}
	run := func(fake *fakeRouterSource, status *router.RouterStatus) *Check {
		t.Helper()
		got, err := routerPortsCheck(context.Background(), doctorOptions{Router: fake}, status)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("fails while the router is absent and a port is held", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.conflicts = []router.PortConflict{conflict}
		got := run(fake, absent)
		if got == nil || got.Status != StatusFail || got.ID != "router.ports" || !strings.Contains(got.Detail, "127.0.0.1:80") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("warns while the router runs unhealthy and a port is held", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.conflicts = []router.PortConflict{conflict}
		if got := run(fake, unhealthy); got == nil || got.Status != StatusWarn {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("is ok when the ports are free", func(t *testing.T) {
		got := run(newFakeRouterSource(), absent)
		if got == nil || got.Status != StatusOK || !strings.HasSuffix(got.Detail, "(the index and ping ports are allocated at its first start)") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("checks nothing for a healthy router, or when Compose did not answer", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.conflicts = []router.PortConflict{conflict}
		if got := run(fake, healthy); got != nil {
			t.Fatalf("healthy: got %+v", got)
		}
		if got := run(fake, nil); got != nil || fake.portsAsked != 0 {
			t.Fatalf("no status: got %+v after %d asks", got, fake.portsAsked)
		}
	})
	t.Run("skips when the ports cannot be checked", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.portsErr = errors.New("Rootless Docker is not supported yet (#121).")
		got := run(fake, absent)
		if got == nil || got.Status != StatusSkip || !strings.Contains(got.Detail, "Rootless Docker") {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRouterStatusChecksShowTheLog(t *testing.T) {
	status := router.RouterStatus{Reachable: true, Running: true, Problems: []router.Problem{
		{Code: "router.not-healthy", Message: "router: not healthy", Log: "line one\nline two"},
	}}
	checks := routerStatusChecks(status)
	if len(checks) != 1 || !strings.Contains(checks[0].Detail, "router: not healthy; last log lines: line one | line two") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestAnswersHTTP(t *testing.T) {
	t.Run("is true for a listener that speaks HTTP", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") }))
		defer server.Close()
		if !answersHTTP(context.Background(), server.Listener.Addr().String()) {
			t.Fatal("an HTTP server was not reported as answering")
		}
	})
	t.Run("is false for a listener that accepts and closes", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback: %v", err)
		}
		defer func() { _ = listener.Close() }()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		if answersHTTP(context.Background(), listener.Addr().String()) {
			t.Fatal("a listener that closes at once was reported as answering")
		}
	})
	t.Run("is false when nothing listens", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback: %v", err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		if answersHTTP(context.Background(), address) {
			t.Fatal("a closed port was reported as answering")
		}
	})
}

func TestOrphanedRoutesChecks(t *testing.T) {
	record := func(t *testing.T, controlHome, repo string, ports map[string]int) {
		t.Helper()
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: repo, WorktreeID: routeWorktree, Slug: "main", Domain: "acme.localhost",
			Initialization: "complete", Ports: ports,
		}); err != nil {
			t.Fatal(err)
		}
	}
	file := func(t *testing.T, state layout.RouterLayout, repo, service string, port int) {
		t.Helper()
		if err := router.WriteFragment(state, repo, routeWorktree, service,
			router.NewFragmentDocument("berth-aaa-"+service, "Host(`"+service+"`)", port)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("says nothing when every route file agrees with its registration", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		got, err := orphanedRoutesChecks(state, registrytest.TempDir(t), "berth")
		if len(got) != 0 || err != nil {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("warns about a file whose registration records another port", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		file(t, state, routeRepo, "api", 30501)
		controlHome := registrytest.TempDir(t)
		record(t, controlHome, routeRepo, map[string]int{"api": 30777})
		got, err := orphanedRoutesChecks(state, controlHome, "berth")
		want := Check{ID: "router.orphaned-routes", Status: StatusWarn, Subject: "routes",
			Detail: "1 route file disagrees with its registration (api: points at port 30501, but its registration records 30777); the next 'berth up' or 'berth router up' repairs it"}
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("counts several repairable files", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		file(t, state, routeRepo, "api", 30501)
		file(t, state, routeRepo, "web", 30501)
		controlHome := registrytest.TempDir(t)
		record(t, controlHome, routeRepo, nil)
		got, err := orphanedRoutesChecks(state, controlHome, "berth")
		if err != nil || len(got) != 1 || !strings.HasPrefix(got[0].Detail, "2 route files disagree with the registrations (") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("reports a file of a repository this control home does not know as a notice, instance other-home", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		file(t, state, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "api", 30501)
		got, err := orphanedRoutesChecks(state, registrytest.TempDir(t), "berth")
		if err != nil || len(got) != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if got[0].ID != "router.orphaned-routes" || got[0].Instance != "other-home" || got[0].Status != StatusOK ||
			!strings.Contains(got[0].Detail, "another control home") || !strings.Contains(got[0].Detail, state.DynamicDir) ||
			!strings.Contains(got[0].Detail, "1 route file matches no registration") {
			t.Fatalf("got %+v", got[0])
		}
	})
	t.Run("warns about a file of a known repository whose worktree has no record, instance unmatched", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		file(t, state, routeRepo, "api", 30501)
		controlHome := registrytest.TempDir(t)
		if err := os.MkdirAll(filepath.Join(controlHome, "repositories", routeRepo), 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := orphanedRoutesChecks(state, controlHome, "berth")
		if err != nil || len(got) != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if got[0].Instance != "unmatched" || got[0].Status != StatusWarn || !strings.Contains(got[0].Detail, "probably stale") {
			t.Fatalf("got %+v", got[0])
		}
	})
	t.Run("agrees its pronouns with the count of unmatched files", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		other := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		file(t, state, other, "api", 30501)
		single, err := orphanedRoutesChecks(state, registrytest.TempDir(t), "berth")
		if err != nil || len(single) != 1 {
			t.Fatalf("got %+v, %v", single, err)
		}
		for _, want := range []string{"nothing removes it", "delete it from", "its worktree is gone"} {
			if !strings.Contains(single[0].Detail, want) {
				t.Errorf("singular detail lacks %q: %s", want, single[0].Detail)
			}
		}
		file(t, state, other, "web", 30502)
		plural, err := orphanedRoutesChecks(state, registrytest.TempDir(t), "berth")
		if err != nil || len(plural) != 1 {
			t.Fatalf("got %+v, %v", plural, err)
		}
		for _, want := range []string{"2 route files match", "nothing removes them", "delete them from", "their worktrees are gone"} {
			if !strings.Contains(plural[0].Detail, want) {
				t.Errorf("plural detail lacks %q: %s", want, plural[0].Detail)
			}
		}
	})
	t.Run("reports repairable and unmatched files together, as two lines", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
		file(t, state, routeRepo, "api", 30501)
		file(t, state, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "web", 30501)
		controlHome := registrytest.TempDir(t)
		record(t, controlHome, routeRepo, map[string]int{"api": 30777})
		got, err := orphanedRoutesChecks(state, controlHome, "berth")
		if err != nil || len(got) != 2 || got[0].Instance != "" || got[1].Instance != "other-home" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestRouterSectionProbes(t *testing.T) {
	both := dockerState{engineAnswered: true, composeAnswered: true}
	ids := func(checks []Check) []string {
		out := make([]string, len(checks))
		for i, check := range checks {
			out[i] = check.ID
		}
		return out
	}
	equal := func(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

	t.Run("checks mode, bind, routes and each route in order when the router runs", func(t *testing.T) {
		state := routerStateWithRoutes(t, map[string]string{"web": "Host(`web.main.acme.localhost`)"})
		var asked atomic.Int32
		opts := testOpts(t, func(o *doctorOptions) {
			o.RouterState = state
			o.RouteClient = routeClientFunc(func(*http.Request) (*http.Response, error) {
				asked.Add(1)
				return plainResponse(200, "text/plain", "ok"), nil
			})
			o.LANProbe = func(context.Context, int) ([]string, error) { return nil, nil }
			o.RouteProbeBudget, o.RouteProbeInterval = 20*time.Millisecond, time.Millisecond
		})
		checks, err := routerSection(context.Background(), opts, both)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"router.container", "router.mode", "router.bind", "router.routes", "router.orphaned-routes", "router.route-probe", "router.route-probe", "router.port"}
		if !equal(ids(checks), want) {
			t.Fatalf("ids = %v, want %v", ids(checks), want)
		}
		if asked.Load() != 2 {
			t.Fatalf("asked %d routes, want 2 (the index and web)", asked.Load())
		}
		if checks[5].Instance != "berth.localhost" || checks[6].Instance != "web.main.acme.localhost" {
			t.Fatalf("probe instances = %q, %q", checks[5].Instance, checks[6].Instance)
		}
	})

	t.Run("probes and checks the bind of nothing when the router is not running", func(t *testing.T) {
		state := routerStateWithRoutes(t, map[string]string{"web": "Host(`web.main.acme.localhost`)"})
		fake := newFakeRouterSource()
		fake.status = router.RouterStatus{Reachable: true, Running: false, Port: 80, Problems: []router.Problem{{Code: "router.absent", Message: "the router is not running"}}}
		var asked atomic.Int32
		opts := testOpts(t, func(o *doctorOptions) {
			o.Router = fake
			o.RouterState = state
			o.RouteClient = routeClientFunc(func(*http.Request) (*http.Response, error) {
				asked.Add(1)
				return plainResponse(200, "", ""), nil
			})
			o.LANProbe = func(context.Context, int) ([]string, error) {
				t.Error("the bind was checked with no router running")
				return nil, nil
			}
		})
		checks, err := routerSection(context.Background(), opts, both)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"router.container", "router.mode", "router.ports", "router.routes", "router.orphaned-routes", "router.port"}
		if !equal(ids(checks), want) {
			t.Fatalf("ids = %v, want %v", ids(checks), want)
		}
		if asked.Load() != 0 {
			t.Fatalf("probed %d routes with no router running", asked.Load())
		}
	})

	t.Run("checks the router's ports right after the mode while it is absent", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.status = router.RouterStatus{Reachable: true, Running: false, Port: 80, Problems: []router.Problem{{Code: "router.absent", Message: "the router is not running"}}}
		opts := testOpts(t, func(o *doctorOptions) {
			o.Router = fake
			o.RouterState = routerStateWithRoutes(t, nil)
		})
		checks, err := routerSection(context.Background(), opts, both)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"router.container", "router.mode", "router.ports", "router.routes", "router.port"}
		if !equal(ids(checks), want) {
			t.Fatalf("ids = %v, want %v", ids(checks), want)
		}
	})

	t.Run("probes the port the running router serves, not the one resolved", func(t *testing.T) {
		state := routerStateWithRoutes(t, nil)
		fake := newFakeRouterSource()
		fake.status.RunningPort = 8081
		var seen atomic.Value
		opts := testOpts(t, func(o *doctorOptions) {
			o.Router = fake
			o.RouterState = state
			o.RouteClient = routeClientFunc(func(r *http.Request) (*http.Response, error) {
				seen.Store(r.URL.Host)
				return plainResponse(200, "", ""), nil
			})
		})
		if _, err := routerSection(context.Background(), opts, both); err != nil {
			t.Fatal(err)
		}
		if seen.Load() != "127.0.0.1:8081" {
			t.Fatalf("probed %v", seen.Load())
		}
	})

	t.Run("ends the command when the context is cancelled during a probe", func(t *testing.T) {
		state := routerStateWithRoutes(t, nil)
		ctx, cancel := context.WithCancelCause(context.Background())
		opts := testOpts(t, func(o *doctorOptions) {
			o.RouterState = state
			o.RouteClient = routeClientFunc(func(*http.Request) (*http.Response, error) {
				cancel(&clierr.Cancelled{Signal: syscall.SIGINT})
				return traefikNotFound(), nil
			})
		})
		if _, err := routerSection(ctx, opts, both); err == nil || err.Error() != "Operation cancelled by SIGINT." {
			t.Fatalf("err = %v", err)
		}
	})
}
