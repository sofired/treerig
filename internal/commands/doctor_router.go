package commands

// The proxy checks of `doctor`'s Router section (docs/design/routing.md,
// decision 13): the mode it runs in, that it answers on loopback alone, and
// that each route answers. There is no Traefik API, so the response itself
// is the evidence: the proxy's own 404 means the route is not loaded, and a
// 502, 503 or 504 means the route is loaded but its target is down or
// unreachable. Like the rest of doctor, nothing here takes a lock or writes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/text"
)

// routeProbeBudget is how long one route gets to start answering. Traefik's
// file provider picks a change up after a throttle of about 2 s, so a route
// written a moment ago needs at least that and some margin.
const routeProbeBudget = 8 * time.Second

// routeProbeInterval is the pause between attempts at one route.
const routeProbeInterval = 500 * time.Millisecond

// routeRequestTimeout bounds one request of a route probe: a target the
// firewall drops hangs the request rather than failing it.
const routeRequestTimeout = 3 * time.Second

// routeProbeParallelism bounds how many routes are probed at once.
const routeProbeParallelism = 8

// routeClient is the part of *http.Client the route probe uses. Tests supply
// their own.
type routeClient interface {
	Do(request *http.Request) (*http.Response, error)
}

// lanProbe reports which of this machine's non-loopback addresses answer
// HTTP on port.
type lanProbe func(ctx context.Context, port int) ([]string, error)

// routeAnswer is how a route's probe ended.
type routeAnswer int

const (
	// answerOK is any response that is not the proxy's own failure: the
	// route is loaded and its target answered.
	answerOK routeAnswer = iota
	// answerServiceNotFound is a 404 the target itself gives for the same
	// request: the route is loaded and the service has nothing at that path.
	answerServiceNotFound
	// answerNotLoaded is a 404 from the router that the target does not give
	// for the same request: no route matches.
	answerNotLoaded
	// answerTargetDown is a 502, 503 or 504 from the router, or a target that
	// does not answer a direct request: the route is loaded and its target
	// did not answer.
	answerTargetDown
	// answerNoProxy is no HTTP answer at all from the router's port.
	answerNoProxy
)

// maxDownRetries is how many times a route whose target is down, or whose
// router does not answer, is asked again. A reboot leaves many stopped
// services, and waiting out each one would stall the report.
const maxDownRetries = 2

// routeTarget is one route to probe.
type routeTarget struct {
	// claim is the route as users know it, "<host>" or "<host><path>".
	claim string
	host  string
	path  string
	// targetPort is the loopback port the route file sends to; 0 when it
	// names none.
	targetPort int
	// hostRoute marks a host route (#6): its target is a process berth does
	// not run, so a target that is down is the normal state, not a warning.
	hostRoute bool
}

// routeTargets are the routes the proxy is expected to serve: the index and
// every route file, services', host routes' and shared services' included, sorted by claim. A rule berth did not write is not
// probed.
func routeTargets(state layout.RouterLayout) ([]routeTarget, error) {
	fragments, err := router.ListFragments(state)
	if err != nil {
		return nil, err
	}
	// Host routes (#6) are probed too: the request through the proxy is the
	// only check of its hop to the host process.
	hostFragments, err := router.ListHostFragments(state)
	if err != nil {
		return nil, err
	}
	fragments = append(fragments, hostFragments...)
	sharedFragments, err := router.ListSharedFragments(state)
	if err != nil {
		return nil, err
	}
	paths := []string{router.IndexFragmentPath(state)}
	hostPaths := map[string]bool{}
	for _, fragment := range fragments {
		paths = append(paths, fragment.Path)
		if fragment.HostRoute {
			hostPaths[fragment.Path] = true
		}
	}
	// A shared service's web UI is probed like any other route.
	for _, fragment := range sharedFragments {
		paths = append(paths, fragment.Path)
	}
	seen := map[string]bool{}
	var targets []routeTarget
	for _, path := range paths {
		// A file that cannot be read safely is skipped, as readFragmentRules does.
		doc, err := router.ReadFragment(state, path)
		if err != nil {
			continue
		}
		for _, entry := range doc.HTTP.Routers {
			match := routeClaimPattern.FindStringSubmatch(entry.Rule)
			if match == nil {
				continue
			}
			claim := match[1] + match[2]
			if seen[claim] {
				continue
			}
			seen[claim] = true
			requestPath := match[2]
			if requestPath == "" {
				requestPath = "/"
			}
			targets = append(targets, routeTarget{claim: claim, host: match[1], path: requestPath, targetPort: doc.TargetPort(), hostRoute: hostPaths[path]})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].claim < targets[j].claim })
	return targets, nil
}

// routeProbeResult is the last answer a route gave.
type routeProbeResult struct {
	answer  routeAnswer
	status  int
	err     error
	elapsed time.Duration
}

// fetch sends target's request, its Host and path, to 127.0.0.1:port and
// returns the status.
func fetch(ctx context.Context, client routeClient, port int, target routeTarget) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+target.path, nil)
	if err != nil {
		return 0, err
	}
	request.Host = target.host
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 256))
	_ = response.Body.Close()
	return response.StatusCode, nil
}

// probeOnce sends one request for target through the router on port and says
// what the answer means. A 404 is the router's own unless the target gives
// the same status for the same request, so the same request is sent straight
// to the route file's target: the same 404 there is the service's own.
func probeOnce(ctx context.Context, client routeClient, port int, target routeTarget) (routeAnswer, int, error) {
	status, err := fetch(ctx, client, port, target)
	if err != nil {
		return answerNoProxy, 0, err
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return answerTargetDown, status, nil
	case http.StatusNotFound:
		if target.targetPort == 0 {
			return answerNotLoaded, status, nil
		}
		direct, err := fetch(ctx, client, target.targetPort, target)
		switch {
		case err != nil:
			return answerTargetDown, status, nil
		case direct == status:
			return answerServiceNotFound, status, nil
		}
		return answerNotLoaded, status, nil
	}
	return answerOK, status, nil
}

// probeRoute asks the router for target until the answer can no longer
// change. A route the router answers 404 for while its target does not
// (the file provider's throttle has not picked it up yet) is retried until
// budget has passed. A router that does not answer, or a target that is
// down, is retried at most maxDownRetries times: those rarely clear within
// the report. A route that answers is done at once; the last answer stands
// otherwise.
func probeRoute(ctx context.Context, client routeClient, port int, target routeTarget, budget, interval time.Duration) routeProbeResult {
	started := time.Now()
	downRetries := 0
	for {
		answer, status, err := probeOnce(ctx, client, port, target)
		result := routeProbeResult{answer: answer, status: status, err: err, elapsed: time.Since(started)}
		if answer == answerOK || answer == answerServiceNotFound || ctx.Err() != nil || result.elapsed+interval >= budget {
			return result
		}
		if answer == answerTargetDown || answer == answerNoProxy {
			downRetries++
			if downRetries > maxDownRetries {
				return result
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result
		case <-timer.C:
		}
	}
}

// routeProbeCheck turns a route's last answer into a check.
func routeProbeCheck(target routeTarget, port int, result routeProbeResult) Check {
	check := Check{ID: "router.route-probe", Instance: target.claim, Subject: "routes"}
	seconds := int(result.elapsed.Round(time.Second) / time.Second)
	switch result.answer {
	case answerOK:
		check.Status, check.Detail = StatusOK, fmt.Sprintf("%s answers (HTTP %d)", target.claim, result.status)
	case answerServiceNotFound:
		check.Status, check.Detail = StatusOK, fmt.Sprintf("%s is routed; the service itself answers HTTP %d for this path", target.claim, result.status)
	case answerNotLoaded:
		check.Status, check.Detail = StatusFail, fmt.Sprintf(
			"%s: the router answers 404 for it after %d s but its target does not, so the route is not loaded; its route file may be missing or not valid", target.claim, seconds)
	case answerTargetDown:
		check.Status = StatusWarn
		switch result.status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			if target.hostRoute {
				// Not a warning, in the words checkout.host-routes uses: the
				// route is loaded, and a dev server that is stopped is normal.
				check.Status, check.Detail = StatusOK, fmt.Sprintf(
					"%s: nothing is listening on 127.0.0.1:%d; requests for it get a 502 until a process is", target.claim, target.targetPort)
				break
			}
			check.Detail = fmt.Sprintf(
				"%s: its target is down or unreachable (the router answers %d); the service may be stopped or not listening on its port",
				target.claim, result.status)
		default:
			check.Detail = fmt.Sprintf(
				"%s: the router answers 404 for it and 127.0.0.1:%d does not answer, so the target is down; if it is up and this persists, the route may not be loaded",
				target.claim, target.targetPort)
		}
	default:
		check.Status, check.Detail = StatusFail, fmt.Sprintf(
			"%s: no HTTP answer from the router on 127.0.0.1:%d: %s", target.claim, port, text.OneLine(errorText(result.err), 200))
	}
	return check
}

// errorText is err's message, or a fixed phrase for a missing one.
func errorText(err error) string {
	if err == nil {
		return "no answer"
	}
	return err.Error()
}

// routeProbeChecks probes every route against the router serving port. A
// client of nil skips the probe: a test that builds options by hand never
// reaches the network.
func routeProbeChecks(ctx context.Context, opts doctorOptions, port int) ([]Check, error) {
	if opts.RouteClient == nil {
		return nil, nil
	}
	targets, err := routeTargets(opts.RouterState)
	if err != nil {
		return nil, err
	}
	budget, interval := opts.RouteProbeBudget, opts.RouteProbeInterval
	if budget <= 0 {
		budget = routeProbeBudget
	}
	if interval <= 0 {
		interval = routeProbeInterval
	}
	checks := make([]Check, len(targets))
	var group errgroup.Group
	group.SetLimit(routeProbeParallelism)
	for i, target := range targets {
		group.Go(func() error {
			checks[i] = routeProbeCheck(target, port, probeRoute(ctx, opts.RouteClient, port, target, budget, interval))
			return nil
		})
	}
	// The goroutines never fail; Wait only joins them.
	_ = group.Wait()
	// A cancelled run ends the command; it is not a line in the report.
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return checks, nil
}

// modeMessageCap is the length the mode check's message is cut to: a
// refusal's advice must survive it.
const modeMessageCap = 1000

// routerProtocolProblem is the status's protocol mismatch, if it has one.
func routerProtocolProblem(status *router.RouterStatus) (router.Problem, bool) {
	if status == nil {
		return router.Problem{}, false
	}
	for _, problem := range status.Problems {
		if problem.Code == "router.protocol" {
			return problem, true
		}
	}
	return router.Problem{}, false
}

// routerModeCheck reports the mode the router runs in, from its container's
// label, against the mode this build would start it in. A daemon berth
// refuses (remote, rootless, unrecognized) is a failure whether or not a
// router is running: nothing berth starts can work there. status is nil when
// Compose did not answer or its status could not be read, which says nothing
// about whether a router runs. A [router] mode override is named, and a
// warning when detection alone would pick another mode.
func routerModeCheck(ctx context.Context, opts doctorOptions, status *router.RouterStatus) (Check, error) {
	check := Check{ID: "router.mode", Subject: "router mode", Status: StatusOK}
	choice, err := opts.Router.ModeChoice(ctx)
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return Check{}, err
		}
		check.Status, check.Detail = StatusFail, text.OneLine(err.Error(), modeMessageCap)
		return check, nil
	}
	source := ""
	if choice.Override != "" {
		source = " (mode set by settings.toml)"
		if choice.Detected != "" && choice.Detected != choice.Override {
			check.Status = StatusWarn
			source = fmt.Sprintf(" (mode set by settings.toml; detection from docker info would pick %s)", choice.Detected)
		}
	}
	switch {
	case status == nil:
		check.Detail = fmt.Sprintf("would start in %s mode%s; whether it runs is unknown, as its status could not be read", choice.Mode, source)
	case !status.Running:
		check.Detail = fmt.Sprintf("not running; would start in %s mode%s", choice.Mode, source)
	default:
		port := status.RunningPort
		if port == 0 {
			port = status.Port
		}
		if problem, mismatched := routerProtocolProblem(status); mismatched {
			check.Status = StatusWarn
			check.Detail = fmt.Sprintf("running in %s mode on 127.0.0.1:%d, but its layout is not this version's: %s",
				orUnknown(status.Mode), port, text.OneLine(problem.Message, modeMessageCap))
			return check, nil
		}
		switch {
		case status.Mode == "":
			check.Status = StatusWarn
			check.Detail = fmt.Sprintf("the running router does not say which mode it is in; this build would use %s mode%s, and the next '%s up' or '%s router up' recreates it",
				choice.Mode, source, opts.CommandName, opts.CommandName)
		case status.Mode != choice.Mode:
			check.Status = StatusWarn
			check.Detail = fmt.Sprintf("running in %s mode on 127.0.0.1:%d, but this build would use %s mode%s; the next '%s up' or '%s router up' recreates it",
				status.Mode, port, choice.Mode, source, opts.CommandName, opts.CommandName)
		default:
			check.Detail = fmt.Sprintf("%s mode, serving 127.0.0.1:%d%s", status.Mode, port, source)
		}
	}
	return check, nil
}

// orUnknown is mode, or "an unknown" for a router that carries no mode label.
func orUnknown(mode router.Mode) string {
	if mode == "" {
		return "an unknown"
	}
	return string(mode)
}

// routerPortsCheck checks that the ports the router needs are free while it
// is not running (a failure: `up` would refuse) and while it is running but
// unhealthy (a warning: the ports may be why). A healthy router holds its own
// ports, so nothing is checked. status is nil when Compose did not answer,
// which skips the check. A daemon berth refuses is the mode check's to
// report, not this one's.
func routerPortsCheck(ctx context.Context, opts doctorOptions, status *router.RouterStatus) (*Check, error) {
	if status == nil || (status.Running && len(status.Problems) == 0) {
		return nil, nil
	}
	conflicts, err := opts.Router.CheckPorts(ctx)
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) || ctx.Err() != nil {
			return nil, err
		}
		return &Check{Status: StatusSkip, ID: "router.ports", Subject: "router ports", Detail: text.OneLine(err.Error(), modeMessageCap)}, nil
	}
	if len(conflicts) == 0 {
		return &Check{Status: StatusOK, ID: "router.ports", Subject: "router ports", Detail: "the ports the router needs are free (the index and ping ports are allocated at its first start)"}, nil
	}
	lines := make([]string, len(conflicts))
	for i, conflict := range conflicts {
		lines[i] = conflict.Describe()
	}
	verdict := StatusFail
	if status.Running {
		verdict = StatusWarn
	}
	return &Check{Status: verdict, ID: "router.ports", Subject: "router ports", Detail: text.OneLine(strings.Join(lines, "; "), 600)}, nil
}

// routerBindCheck proves the router answers on loopback alone: no other
// address of this machine may answer HTTP on its port. A probe of nil skips
// the check. A cancelled context ends the command: it proves nothing.
func routerBindCheck(ctx context.Context, opts doctorOptions, port int) (*Check, error) {
	if opts.LANProbe == nil {
		return nil, nil
	}
	answering, err := opts.LANProbe(ctx, port)
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, err
	}
	if len(answering) > 0 {
		return &Check{Status: StatusFail, ID: "router.bind", Subject: "router bind", Detail: fmt.Sprintf(
			"port %d also answers on %s; the router must be reachable on 127.0.0.1 alone",
			port, text.OneLine(strings.Join(answering, ", "), 200))}, nil
	}
	return &Check{Status: StatusOK, ID: "router.bind", Subject: "router bind", Detail: fmt.Sprintf(
		"127.0.0.1:%d only; no other local address answers", port)}, nil
}

// lanDialTimeout bounds one attempt at one local address.
const lanDialTimeout = 500 * time.Millisecond

// lanAnswering is the real lanProbe: it asks every non-loopback unicast
// address of this machine, concurrently, for "/" on port and reports the ones
// that answer with an HTTP response. A connection that is accepted and then
// closed is not an answer, which is how OrbStack refuses a LAN connection it
// listens for. A cancelled context returns its cause, not an empty answer.
func lanAnswering(ctx context.Context, port int) ([]string, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing this machine's addresses: %w", err)
	}
	var targets []string
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || !network.IP.IsGlobalUnicast() {
			continue
		}
		targets = append(targets, net.JoinHostPort(network.IP.String(), strconv.Itoa(port)))
	}
	replies := make([]bool, len(targets))
	var group errgroup.Group
	group.SetLimit(routeProbeParallelism)
	for i, target := range targets {
		group.Go(func() error {
			replies[i] = answersHTTP(ctx, target)
			return nil
		})
	}
	// The goroutines never fail; Wait only joins them.
	_ = group.Wait()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	var answering []string
	for i, answered := range replies {
		if answered {
			answering = append(answering, targets[i])
		}
	}
	return answering, nil
}

// answersHTTP reports whether target responds to a request with HTTP.
func answersHTTP(ctx context.Context, target string) bool {
	dialer := net.Dialer{Timeout: lanDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(lanDialTimeout)); err != nil {
		return false
	}
	if _, err := io.WriteString(conn, "HEAD / HTTP/1.0\r\nHost: probe\r\n\r\n"); err != nil {
		return false
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return false
	}
	return string(reply) == "HTTP/"
}
