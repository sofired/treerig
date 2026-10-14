package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/worktree"
)

// DefaultWaitTimeoutSeconds is how long `up --wait` waits for health before failing.
const DefaultWaitTimeoutSeconds = 120

// RouteName is the Traefik router/service name one routed service carries:
// <project>-<service>, namespaced by the worktree's Compose project so it is
// unique across every worktree the shared router serves. It names the router
// and the service in the route file internal/commands's UpWithContext writes
// (via internal/router's WriteFragment).
func RouteName(ref worktree.Ref, route RouteSpec) string {
	return layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID) + "-" + route.Service
}

// RouteRule is the Traefik routing rule one route matches: Host(`<host>`),
// with the route's own subdomain prefixed onto host when it has one, and
// `&& PathPrefix(`<path>`)` appended when it names a path. Shared by
// whoever writes that route's dynamic-configuration fragment, so the rule
// string is built in exactly one place.
func RouteRule(route RouteSpec, host string) string {
	routeHost := host
	if route.Subdomain != "" {
		routeHost = route.Subdomain + "." + host
	}
	rule := fmt.Sprintf("Host(`%s`)", routeHost)
	if route.Path != nil {
		rule += fmt.Sprintf(" && PathPrefix(`%s`)", *route.Path)
	}
	return rule
}

// RouteSpec is one [service.<name>] table, as the runtime needs it for routing.
type RouteSpec struct {
	Service string
	// Subdomain empty routes at the worktree host itself.
	Subdomain string
	// Path, if set, routes only this prefix, so several services can share one host.
	Path *string
	// Port is the container port; nil defaults to the first the service exposes or publishes.
	Port *int
	// HostPort, when set, is the loopback port (#119) the override publishes
	// the service's routed container port on, at 127.0.0.1. Zero publishes
	// nothing.
	HostPort int
}

// loopbackHost is the host address a routed service's port is published on.
const loopbackHost = "127.0.0.1"

// home is the worktree's own directory, where its override document is written.
func (c *Compose) home(ref worktree.Ref) string {
	return layout.WorktreeLayout(c.options.ControlHome, ref.RepositoryID, ref.WorktreeID).Home
}

// overridePath is JSON content under a .yaml name: JSON is valid YAML,
// Compose reads it as such, and it spares this build a YAML emitter and
// every quoting bug one brings.
func (c *Compose) overridePath(ref worktree.Ref) string {
	return filepath.Join(c.home(ref), brand.Name+".override.yaml")
}

// ownershipLabels are the labels stamped onto the resources this worktree's
// Compose file creates, so Inventory can find and verify them later: managed,
// repository and worktree, which prove who owns a resource, and, with
// withFormat, the format label, which says what the other three mean
// (docs/design/state.md). Named volumes get no format label: Compose treats
// a changed label on an existing volume as drift and offers to recreate it,
// losing its data, so a volume is proven by the three ownership labels alone.
func (c *Compose) ownershipLabels(ref worktree.Ref, withFormat bool) *jsonKV {
	namespace := c.options.LabelNamespace
	labels := newJSONKV().
		set(brand.ManagedLabel(namespace), "true").
		set(brand.RepositoryLabel(namespace), ref.RepositoryID).
		set(brand.WorktreeLabel(namespace), ref.WorktreeID)
	if withFormat {
		labels.set(brand.FormatLabel(namespace), strconv.Itoa(stateformat.DockerLabel))
	}
	return labels
}

// composeConfigJSON is `docker compose config --format json` against the
// consumer's file alone, shared by readModel (the lossy map form Status
// uses) and readOrderedModel (the order-preserving form Up needs).
func (c *Compose) composeConfigJSON(ctx context.Context, ref worktree.Ref) (string, error) {
	file := c.options.ComposeFile
	result, err := c.compose(ctx, ref, []string{"config", "--format", "json"}, []string{file},
		run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", clierr.Newf("Compose could not read %s:\n%s", file, firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)))
	}
	return result.Stdout, nil
}

// readOrderedModel is the consumer's Compose file as Compose itself
// resolves it, decoded with every object's key order preserved: the
// override document's own key order must follow it exactly. Reads nothing
// else, writes nothing.
func (c *Compose) readOrderedModel(ctx context.Context, ref worktree.Ref) (services, volumes, networks *jsonKV, err error) {
	stdout, err := c.composeConfigJSON(ctx, ref)
	if err != nil {
		return nil, nil, nil, err
	}
	return decodeOrderedComposeModel([]byte(stdout), c.options.ComposeFile)
}

// decodeOrderedComposeModel is the ordered decode of one `docker compose
// config --format json` document into its services, volumes, and networks
// sections, each defaulting to an empty object when absent. Every entry
// within a section must itself be an object, and every field
// overrideDocument reads off it must match the shape it expects — a
// service's build (an object, if present), expose (an array of strings
// and/or numbers, if present), and ports (an array of objects whose target,
// if present, is a number); a volume's or network's external (a boolean or
// an object, if present — never null, a string, or a number). Anything else
// is refused outright as an unrecognized document, before overrideDocument
// ever sees the data — a mismatch here must refuse just as completely, not
// read the malformed field as whatever isExternal or firstPort would make
// of it.
func decodeOrderedComposeModel(data []byte, file string) (services, volumes, networks *jsonKV, err error) {
	root, decodeErr := decodeOrdered(data)
	if decodeErr != nil {
		return nil, nil, nil, clierr.Newf("Compose returned invalid JSON for %s.", file)
	}
	obj, ok := root.(*jsonKV)
	if !ok {
		return nil, nil, nil, clierr.Newf("Compose returned an unrecognized document for %s.", file)
	}
	unrecognized := clierr.Newf("Compose returned an unrecognized document for %s.", file)

	if services, err = orderedSection(obj, "services", file); err != nil {
		return nil, nil, nil, err
	}
	for _, name := range services.keys {
		service, _ := services.values[name].(*jsonKV)
		if !validateServiceShape(service) {
			return nil, nil, nil, unrecognized
		}
	}

	if volumes, err = orderedSection(obj, "volumes", file); err != nil {
		return nil, nil, nil, err
	}
	for _, name := range volumes.keys {
		volume, _ := volumes.values[name].(*jsonKV)
		if !validateExternalShape(volume) {
			return nil, nil, nil, unrecognized
		}
	}

	if networks, err = orderedSection(obj, "networks", file); err != nil {
		return nil, nil, nil, err
	}
	for _, name := range networks.keys {
		network, _ := networks.values[name].(*jsonKV)
		if !validateExternalShape(network) {
			return nil, nil, nil, unrecognized
		}
	}

	return services, volumes, networks, nil
}

// orderedSection is obj's key as a *jsonKV of *jsonKV entries — services,
// volumes or networks, say — or an empty one if key is absent. Anything else
// shaped there is refused as an unrecognized document.
func orderedSection(obj *jsonKV, key, file string) (*jsonKV, error) {
	value, present := obj.get(key)
	if !present {
		return newJSONKV(), nil
	}
	section, ok := value.(*jsonKV)
	if !ok {
		return nil, clierr.Newf("Compose returned an unrecognized document for %s.", file)
	}
	for _, name := range section.keys {
		if _, ok := section.values[name].(*jsonKV); !ok {
			return nil, clierr.Newf("Compose returned an unrecognized document for %s.", file)
		}
	}
	return section, nil
}

// validateServiceShape checks one service entry against the subset of
// composeConfigSchema's per-service shape that overrideDocument and
// firstPort read: build (z.record: an object, if present), expose (z.array
// of z.union([z.string(), z.number()]), if present), and ports (z.array of
// objects whose target, z.number().optional(), is numeric when present).
// Every other field is z.unknown() or passthrough and needs no check.
func validateServiceShape(service *jsonKV) bool {
	if buildValue, present := service.get("build"); present {
		if _, ok := buildValue.(*jsonKV); !ok {
			return false
		}
	}
	if exposeValue, present := service.get("expose"); present {
		array, ok := exposeValue.([]any)
		if !ok {
			return false
		}
		for _, item := range array {
			switch item.(type) {
			case string, json.Number:
			default:
				return false
			}
		}
	}
	if portsValue, present := service.get("ports"); present {
		array, ok := portsValue.([]any)
		if !ok {
			return false
		}
		for _, item := range array {
			entry, ok := item.(*jsonKV)
			if !ok {
				return false
			}
			if target, present := entry.get("target"); present {
				if _, ok := target.(json.Number); !ok {
					return false
				}
			}
		}
	}
	return true
}

// validateExternalShape checks one volume or network entry's external
// field against z.union([z.boolean(), z.object({}).passthrough()]).
// optional(): absent is fine (undefined), and so is a boolean or an object,
// but nothing else — notably not null, which zod's object branch refuses
// same as any other non-object, non-undefined value.
func validateExternalShape(resource *jsonKV) bool {
	value, present := resource.get("external")
	if !present {
		return true
	}
	switch value.(type) {
	case bool:
		return true
	case *jsonKV:
		return true
	default:
		return false
	}
}

// isExternal reports whether resource's "external" key marks it as external:
// the key must be present (a missing key is never external) and not exactly
// boolean false — an explicit external: null counts as external.
func isExternal(resource *jsonKV) bool {
	value, present := resource.get("external")
	if !present {
		return false
	}
	if b, ok := value.(bool); ok && !b {
		return false
	}
	return true
}

// usesDefaultNetwork reports whether a service with no explicit `networks`
// key, or one naming "default", joins the default network.
func usesDefaultNetwork(service *jsonKV) bool {
	networks, present := service.get("networks")
	if !present || networks == nil {
		return true
	}
	switch v := networks.(type) {
	case []any:
		for _, item := range v {
			if name, ok := item.(string); ok && name == "default" {
				return true
			}
		}
		return false
	case *jsonKV:
		return v.has("default")
	default:
		return false
	}
}

// networkNameString is value the way JavaScript's String() would stringify a
// decoded JSON value used as a network name: "null" for null, "true"/"false"
// for a boolean, and the value itself for anything already a string.
func networkNameString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// firstPort ports Number.parseInt(String(expose[0]).split('/')[0], 10):
// the first `expose` entry's leading digits, stopping at the first
// non-digit (so "8080/tcp" reads as 8080), falling back to the first
// `ports` entry that declares a target.
func firstPort(service *jsonKV) (int, bool) {
	if exposeValue, present := service.get("expose"); present {
		if array, ok := exposeValue.([]any); ok && len(array) > 0 {
			if port, ok := parseLeadingInt(stringifyJSONValue(array[0])); ok {
				return port, true
			}
		}
	}
	if portsValue, present := service.get("ports"); present {
		if array, ok := portsValue.([]any); ok {
			for _, entry := range array {
				entryObj, ok := entry.(*jsonKV)
				if !ok {
					continue
				}
				if target, present := entryObj.get("target"); present {
					if port, ok := numberToInt(target); ok {
						return port, true
					}
				}
			}
		}
	}
	return 0, false
}

// stringifyJSONValue is value the way JavaScript's String() would: a string
// passes through unchanged, everything else falls to networkNameString.
func stringifyJSONValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return networkNameString(v)
	}
}

// numberToInt reads value as a whole number: a decoded JSON number directly,
// or anything else by parsing its leading digits the way firstPort's own
// Number.parseInt port does.
func numberToInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return parseLeadingInt(stringifyJSONValue(v))
	}
}

// parseLeadingInt ports Number.parseInt(s, 10): leading whitespace, an
// optional sign, then as many decimal digits as there are; no digits at all
// is the "NaN" case, reported as (0, false).
func parseLeadingInt(s string) (int, bool) {
	i, n := 0, len(s)
	for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	start := i
	if i < n && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digitsStart := i
	for i < n && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == digitsStart {
		return 0, false
	}
	value, err := strconv.Atoi(s[start:i])
	if err != nil {
		return 0, false
	}
	return value, true
}

// overrideDocument is pure: the override that stamps ownership labels onto
// every service, every built image, and every non-external volume and
// network, and routes the declared services. Every ordered map here is
// built with jsonKV.set, which behaves like a JavaScript property
// assignment (overwrite in place, append if new) — see jsonKV's own
// comment.
func (c *Compose) overrideDocument(ref worktree.Ref, services, volumes, networks *jsonKV) (*jsonKV, error) {
	labels := c.ownershipLabels(ref, true)
	volumeLabels := c.ownershipLabels(ref, false)
	stamp := func() *jsonKV { return newJSONKV().set("labels", labels) }

	outServices := newJSONKV()
	for _, name := range services.keys {
		service, _ := services.values[name].(*jsonKV)
		entry := stamp()
		if uses := c.options.SharedUse[name]; len(uses) > 0 {
			entry.set("labels", c.withSharedUse(labels, uses))
		}
		// Service labels reach the container; only build.labels reach the image it builds.
		if service.has("build") {
			entry.set("build", newJSONKV().set("labels", labels))
		}
		outServices.set(name, entry)
	}

	outVolumes := newJSONKV()
	for _, name := range volumes.keys {
		volume, _ := volumes.values[name].(*jsonKV)
		if isExternal(volume) {
			continue
		}
		outVolumes.set(name, newJSONKV().set("labels", volumeLabels))
	}

	outNetworks := newJSONKV()
	for _, name := range networks.keys {
		network, _ := networks.values[name].(*jsonKV)
		if isExternal(network) {
			continue
		}
		outNetworks.set(name, stamp())
	}

	defaultDeclared := networks.has("default")
	anyUsesDefault := false
	for _, name := range services.keys {
		service, _ := services.values[name].(*jsonKV)
		if usesDefaultNetwork(service) {
			anyUsesDefault = true
			break
		}
	}
	if !defaultDeclared && anyUsesDefault {
		outNetworks.set("default", stamp())
	}

	// A routed service publishes its pinned loopback port (#119) and nothing
	// else: no network is added, and no alias, because the proxy reaches the
	// service at 127.0.0.1:<port> (docs/design/routing.md). A route is
	// refused for a service the Compose file does not define, one with no
	// port to publish, and one that cannot publish a port of its own.
	for _, route := range c.options.Routes {
		service, ok := services.values[route.Service].(*jsonKV)
		if !ok {
			return nil, clierr.Newf("[service.%s] names a service that is not defined in %s.", route.Service, c.options.ComposeFile)
		}
		target, ok := resolvePort(route, service)
		if !ok {
			return nil, clierr.Newf(
				"Service %s declares no port: set port in its [service.%s] table, or expose one in %s.",
				route.Service, route.Service, c.options.ComposeFile,
			)
		}
		if err := refuseUnpublishable(route.Service, c.options.ComposeFile, service); err != nil {
			return nil, err
		}
		if route.HostPort > 0 {
			entry, _ := outServices.values[route.Service].(*jsonKV)
			// Compose merges this with the service's own ports; the long
			// syntax pins the address to loopback, which the short one
			// can too but with a string that has to be built and parsed.
			entry.set("ports", []any{newJSONKV().
				set("target", target).
				set("published", strconv.Itoa(route.HostPort)).
				set("host_ip", loopbackHost).
				set("protocol", "tcp")})
		}
	}

	return newJSONKV().set("services", outServices).set("volumes", outVolumes).set("networks", outNetworks), nil
}

// withSharedUse is labels plus a shared-use label for each shared service a
// container uses, in sorted order so the override is the same bytes each time.
func (c *Compose) withSharedUse(labels *jsonKV, uses []string) *jsonKV {
	stamped := newJSONKV()
	for _, key := range labels.keys {
		value, _ := labels.get(key)
		stamped.set(key, value)
	}
	for _, name := range slices.Sorted(slices.Values(uses)) {
		stamped.set(brand.SharedUseLabel(c.options.LabelNamespace, name), "true")
	}
	return stamped
}

// refuseUnpublishable refuses a routed service berth cannot publish one
// loopback port for (docs/design/routing.md, decision 12): a service in the
// host's network namespace or another service's has no port of its own to
// publish, and several replicas cannot share one.
func refuseUnpublishable(name, composeFile string, service *jsonKV) error {
	if mode, ok := service.get("network_mode"); ok {
		if text, ok := mode.(string); ok && (text == "host" || strings.HasPrefix(text, "service:")) {
			return clierr.Newf(
				"Service %s in %s uses network_mode: %s, so %s cannot publish a loopback port for it. "+
					"A routed service needs its own network namespace: remove network_mode, or remove its [service.%s] table.",
				name, composeFile, text, brand.Name, name)
		}
	}
	if replicas := replicaCount(service); replicas > 1 {
		return clierr.Newf(
			"Service %s in %s runs %d replicas, but a routed service publishes one loopback port, which replicas cannot share. "+
				"Run one replica, or remove its [service.%s] table.",
			name, composeFile, replicas, name)
	}
	return nil
}

// replicaCount is how many containers a service asks for: the larger of
// deploy.replicas and scale, either of which Compose accepts; 1 when neither is set.
func replicaCount(service *jsonKV) int {
	count := 1
	read := func(value any, present bool) {
		if !present {
			return
		}
		if number, ok := value.(json.Number); ok {
			if n, err := number.Int64(); err == nil && int(n) > count {
				count = int(n)
			}
		}
	}
	read(service.get("scale"))
	if deploy, ok := service.values["deploy"].(*jsonKV); ok {
		read(deploy.get("replicas"))
	}
	return count
}

// resolvePort is route's explicit port if it named one, otherwise the
// service's own first port, the way firstPort finds it.
func resolvePort(route RouteSpec, service *jsonKV) (int, bool) {
	if route.Port != nil {
		return *route.Port, true
	}
	return firstPort(service)
}

// prepare reads the model and writes the override into the worktree home.
// Only Up calls this: the home must already exist, created by the command
// layer with its containment checks — the runtime never creates it.
func (c *Compose) prepare(ctx context.Context, ref worktree.Ref, selected []string) ([]string, error) {
	home := c.home(ref)
	if err := files.AssertSafeDirectoryPath(c.options.ControlHome, home); err != nil {
		return nil, err
	}
	exists, err := files.PathExists(home)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, clierr.Newf(
			"Worktree home %s does not exist. `up` creates it before starting services, so this is a bug in %s; please report it.",
			home, brand.Name,
		)
	}

	services, volumes, networks, err := c.readOrderedModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := refuseUnlabelledTargets(services, selected); err != nil {
		return nil, err
	}
	document, err := c.overrideDocument(ref, services, volumes, networks)
	if err != nil {
		return nil, err
	}
	encoded, err := marshalOverrideDocument(document)
	if err != nil {
		return nil, err
	}
	override := c.overridePath(ref)
	if err := files.AtomicWrite(override, encoded, 0o600); err != nil {
		return nil, err
	}
	return []string{c.options.ComposeFile, override}, nil
}

// refuseUnlabelledTargets refuses any service named on the command line that
// the resolved model does not contain. `compose config` leaves out a service
// whose profile is not active, so the override never labels it, yet
// `compose up <service>` enables a named service's profile and starts it
// anyway: without this check it would run with none of this worktree's
// ownership labels, and nothing could ever find or remove it.
func refuseUnlabelledTargets(services *jsonKV, selected []string) error {
	var missing []string
	for _, name := range selected {
		if !services.has(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return clierr.Newf(
		"Not starting %s: Compose does not enable it in this configuration, most likely because its profile is not active, so it could not be labelled as this worktree's. Enable its profile (for example with COMPOSE_PROFILES), then run this again.",
		strings.Join(missing, ", "),
	)
}

// marshalOverrideDocument is JSON.stringify(document, null, 2) + "\n":
// compact ordered JSON (marshalOrdered), reformatted with json.Indent —
// which only inserts whitespace between tokens, never touching string
// content — then a trailing newline.
func marshalOverrideDocument(doc *jsonKV) ([]byte, error) {
	compact, err := marshalOrdered(doc)
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

// Up writes the override, then starts services — every declared service
// when services is empty. `compose up` itself deliberately carries no
// Timeout (#14), only its own `--wait-timeout` flag: see compose.go's own
// timeouts comment for why.
func (c *Compose) Up(ctx context.Context, ref worktree.Ref, services []string) error {
	overrideFiles, err := c.prepare(ctx, ref, services)
	if err != nil {
		return err
	}
	if err := c.refuseNewerFormats(ctx, ref); err != nil {
		return err
	}
	timeout := DefaultWaitTimeoutSeconds
	if c.options.WaitTimeoutSeconds != nil {
		timeout = *c.options.WaitTimeoutSeconds
	}
	args := []string{"up", "--detach", "--build", "--remove-orphans", "--wait", "--wait-timeout", strconv.Itoa(timeout)}
	args = append(args, services...)
	_, err = c.compose(ctx, ref, args, overrideFiles, run.Spec{Output: run.Tee})
	return err
}

// refuseNewerFormats refuses to start a worktree that owns a container,
// network or image a newer berth stamped. Up calls it once the override is
// ready and before `compose up`: the override this build writes carries its own
// format label, which Compose sees as a changed configuration and answers by
// recreating what it finds, and `--build` re-tags the worktree's service
// images, so the refusal cannot wait for the ownership proof at teardown. It is
// the cheap check docker.FindNewerFormats makes (two listings, an image listing
// and one inspect only when images exist), not a full Inventory; a resource
// with a missing label is left to that proof.
func (c *Compose) refuseNewerFormats(ctx context.Context, ref worktree.Ref) error {
	namespace := c.options.LabelNamespace
	newer, err := docker.FindNewerFormats(ctx, c.options.Runner, []string{
		brand.ManagedLabel(namespace) + "=true",
		brand.RepositoryLabel(namespace) + "=" + ref.RepositoryID,
		brand.WorktreeLabel(namespace) + "=" + ref.WorktreeID,
	}, brand.FormatLabel(namespace), stateformat.DockerLabel, true, c.timeouts.Probe)
	if err != nil || len(newer) == 0 {
		return err
	}
	return stateformat.Newer("touch", stateformat.DescribeResource(newer[0].Kind, newer[0].ID, ""), newer[0].Found, stateformat.DockerLabel)
}
