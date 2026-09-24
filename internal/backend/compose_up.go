package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// DefaultWaitTimeoutSeconds is how long `up --wait` waits for health before failing.
const DefaultWaitTimeoutSeconds = 120

// routedByLabel and routerEntrypoint mirror src/router/stack.ts's
// ROUTED_BY_LABEL and ROUTER_ENTRYPOINT — the router package itself is
// future work (#39); until it exists to import, these constants live here,
// where routeLabels is the one place that needs them.
const (
	routedByLabel    = brand.LabelNamespace + ".router"
	routerEntrypoint = "web"
)

// RouteSpec is one [[service]] entry, as the runtime needs it for routing.
type RouteSpec struct {
	Service string
	// Subdomain empty routes at the worktree host itself.
	Subdomain string
	// Path, if set, routes only this prefix, so several services can share one host.
	Path *string
	// Port is the container port; nil defaults to the first the service exposes or publishes.
	Port *int
}

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

// ownershipLabels are the three labels stamped onto every resource this
// worktree's Compose file creates, so Inventory can find and verify them later.
func (c *Compose) ownershipLabels(ref worktree.Ref) *jsonKV {
	namespace := c.options.LabelNamespace
	return newJSONKV().
		set(brand.ManagedLabel(namespace), "true").
		set(brand.RepositoryLabel(namespace), ref.RepositoryID).
		set(brand.WorktreeLabel(namespace), ref.WorktreeID)
}

// composeConfigJSON is `docker compose config --format json` against the
// consumer's file alone, shared by readModel (the lossy map form Status
// uses) and readOrderedModel (the order-preserving form Up needs).
func (c *Compose) composeConfigJSON(ctx context.Context, ref worktree.Ref) (string, error) {
	file := c.options.ComposeFile
	result, err := c.compose(ctx, ref, []string{"config", "--format", "json"}, []string{file}, run.Spec{AllowFailure: true})
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
// sections, each defaulting to an empty object when absent, as the
// TypeScript build's composeConfigSchema defaults them. Every entry within a
// section must itself be an object, and every field overrideDocument reads
// off it must match that schema's own nested shape — a service's build (an
// object, if present), expose (an array of strings and/or numbers, if
// present), and ports (an array of objects whose target, if present, is a
// number); a volume's or network's external (a boolean or an object, if
// present; z.union has no room for null, a string, or a number). Anything
// else is the same "unrecognized document" refusal readModel gives, which
// zod's own safeParse would have failed with before overrideDocument ever
// saw the data — a mismatch here must refuse just as completely, not read
// the malformed field as whatever isExternal or firstPort would make of it.
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

// isExternal ports the TypeScript build's own: the external key must be
// present (a missing key is undefined, and undefined is never external) and
// not exactly boolean false — an explicit external: null is external, same
// as TypeScript's own `!== false` check would read it.
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

// serviceNetworks is the networks a service is on, as Compose normalized
// them; no key (or an unrecognized shape) means the implicit default.
func serviceNetworks(service *jsonKV) *jsonKV {
	networks, present := service.get("networks")
	if present {
		if array, ok := networks.([]any); ok {
			result := newJSONKV()
			for _, item := range array {
				result.set(networkNameString(item), newJSONKV())
			}
			return result
		}
		if obj, ok := networks.(*jsonKV); ok && networks != nil {
			result := newJSONKV()
			for _, name := range obj.keys {
				result.set(name, newJSONKV())
			}
			return result
		}
	}
	return newJSONKV().set("default", newJSONKV())
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

// routeLabels are the Traefik labels one routed service carries, in the
// TypeScript build's own insertion order.
func routeLabels(name string, route RouteSpec, network, host string, port int) *jsonKV {
	routeHost := host
	if route.Subdomain != "" {
		routeHost = route.Subdomain + "." + host
	}
	rule := fmt.Sprintf("Host(`%s`)", routeHost)
	if route.Path != nil {
		rule += fmt.Sprintf(" && PathPrefix(`%s`)", *route.Path)
	}
	return newJSONKV().
		set("traefik.enable", "true").
		set("traefik.docker.network", network).
		set(routedByLabel, network).
		set(fmt.Sprintf("traefik.http.routers.%s.rule", name), rule).
		set(fmt.Sprintf("traefik.http.routers.%s.entrypoints", name), routerEntrypoint).
		set(fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", name), strconv.Itoa(port))
}

// overrideDocument is pure: the override that stamps ownership labels onto
// every service, every built image, and every non-external volume and
// network, and routes the declared services. Every ordered map here is
// built with jsonKV.set, which behaves like a JavaScript property
// assignment (overwrite in place, append if new) — see jsonKV's own
// comment — so this follows src/runtime/compose.ts's overrideDocument
// statement for statement, including its edge cases around a router network
// that collides with an already-declared name.
func (c *Compose) overrideDocument(ref worktree.Ref, services, volumes, networks *jsonKV) (*jsonKV, error) {
	labels := c.ownershipLabels(ref)
	stamp := func() *jsonKV { return newJSONKV().set("labels", labels) }

	outServices := newJSONKV()
	for _, name := range services.keys {
		service, _ := services.values[name].(*jsonKV)
		entry := stamp()
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
		outVolumes.set(name, stamp())
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

	if len(c.options.Routes) > 0 {
		host := layout.WorktreeHost(ref.Slug, c.options.Domain)
		network := c.options.RouterNetwork
		for _, route := range c.options.Routes {
			service, ok := services.values[route.Service].(*jsonKV)
			if !ok {
				return nil, clierr.Newf("[[service]] %s is not defined in %s.", route.Service, c.options.ComposeFile)
			}
			port, ok := resolvePort(route, service)
			if !ok {
				return nil, clierr.Newf(
					"Service %s declares no port: set port in its [[service]] entry, or expose one in %s.",
					route.Service, c.options.ComposeFile,
				)
			}
			name := c.project(ref) + "-" + route.Service
			merged := newJSONKV()
			for _, key := range labels.keys {
				value, _ := labels.get(key)
				merged.set(key, value)
			}
			extra := routeLabels(name, route, network, host, port)
			for _, key := range extra.keys {
				value, _ := extra.get(key)
				merged.set(key, value)
			}

			entry, _ := outServices.values[route.Service].(*jsonKV)
			entry.set("labels", merged)
			svcNetworks := serviceNetworks(service)
			svcNetworks.set(network, newJSONKV())
			entry.set("networks", svcNetworks)
		}
		outNetworks.set(network, newJSONKV().set("external", true))
	}

	return newJSONKV().set("services", outServices).set("volumes", outVolumes).set("networks", outNetworks), nil
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
func (c *Compose) prepare(ctx context.Context, ref worktree.Ref) ([]string, error) {
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
// when services is empty.
func (c *Compose) Up(ctx context.Context, ref worktree.Ref, services []string) error {
	overrideFiles, err := c.prepare(ctx, ref)
	if err != nil {
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
