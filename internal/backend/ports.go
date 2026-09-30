package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// PortPublisher is a running container that publishes a host port.
type PortPublisher struct {
	// ID is the container's ID as `docker ps` prints it.
	ID string
	// Name is the container's name, without Docker's leading slash.
	Name string
	// Own is true when the container carries ref's ownership labels — managed,
	// repository and worktree — proven by labels, never by its name.
	Own bool
	// Managed is true when the container carries berth's managed label at
	// all, whichever worktree it names: another berth worktree's container.
	Managed bool
	// Service is the Compose service the container runs, when it has the label.
	Service string
	// LoopbackPorts are the host ports the container publishes on
	// 127.0.0.1, sorted.
	LoopbackPorts []int
}

// PortPublishers lists the running containers that publish host port,
// classified against ref by their ownership labels. It asks Docker with
// `ps --filter publish=<port>`, which matches the host port, and inspects each
// answer once. A container that vanishes between the two calls is not a
// holder any more and is left out.
func (c *Compose) PortPublishers(ctx context.Context, ref worktree.Ref, port int) ([]PortPublisher, error) {
	return c.publishers(ctx, ref, []string{"--filter", "publish=" + strconv.Itoa(port)})
}

// ServicePublishers lists the running containers of service that carry ref's
// ownership labels, each with the loopback ports it actually publishes: what
// `doctor` compares with the recorded port.
func (c *Compose) ServicePublishers(ctx context.Context, ref worktree.Ref, service string) ([]PortPublisher, error) {
	namespace := c.options.LabelNamespace
	return c.publishers(ctx, ref, []string{
		"--filter", "label=" + brand.ManagedLabel(namespace) + "=true",
		"--filter", "label=" + brand.RepositoryLabel(namespace) + "=" + ref.RepositoryID,
		"--filter", "label=" + brand.WorktreeLabel(namespace) + "=" + ref.WorktreeID,
		"--filter", "label=" + ComposeServiceLabel + "=" + service,
	})
}

// publishers lists the running containers `docker ps` selects with filters
// and inspects each once, classifying it against ref.
func (c *Compose) publishers(ctx context.Context, ref worktree.Ref, filters []string) ([]PortPublisher, error) {
	namespace := c.options.LabelNamespace
	listed, err := docker.Run(ctx, c.options.Runner,
		append(append([]string{"ps"}, filters...), "--format", "{{.ID}}"),
		run.Spec{Timeout: c.timeouts.Probe})
	if err != nil {
		return nil, err
	}
	label := func(name string) string { return fmt.Sprintf(`{{ index .Config.Labels "%s" }}`, name) }
	format := strings.Join([]string{
		label(brand.ManagedLabel(namespace)),
		label(brand.RepositoryLabel(namespace)),
		label(brand.WorktreeLabel(namespace)),
		label(ComposeServiceLabel),
		"{{ .Name }}",
		"{{ json .NetworkSettings.Ports }}",
	}, "\t")
	var publishers []PortPublisher
	for _, id := range strings.Fields(listed.Stdout) {
		inspected, err := docker.Run(ctx, c.options.Runner, []string{"inspect", "--format", format, id},
			run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
		if err != nil {
			return nil, err
		}
		if inspected.ExitCode != 0 {
			continue
		}
		fields := strings.Split(strings.TrimRight(inspected.Stdout, "\r\n"), "\t")
		field := func(i int) string {
			if i < len(fields) {
				return fields[i]
			}
			return ""
		}
		name := strings.TrimPrefix(field(4), "/")
		if name == "" {
			name = id
		}
		publishers = append(publishers, PortPublisher{
			ID:            id,
			Name:          name,
			Managed:       field(0) == "true",
			Own:           field(0) == "true" && field(1) == ref.RepositoryID && field(2) == ref.WorktreeID,
			Service:       field(3),
			LoopbackPorts: loopbackPorts(field(5)),
		})
	}
	return publishers, nil
}

// loopbackPorts are the host ports bound on 127.0.0.1 in a container's
// NetworkSettings.Ports document; anything unreadable yields none.
func loopbackPorts(document string) []int {
	var bindings map[string][]struct{ HostIp, HostPort string }
	if err := json.Unmarshal([]byte(document), &bindings); err != nil {
		return nil
	}
	var found []int
	for _, published := range bindings {
		for _, binding := range published {
			if port, err := strconv.Atoi(binding.HostPort); err == nil && binding.HostIp == loopbackHost {
				found = append(found, port)
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// FixedPort is a host port a service's own Compose file publishes.
type FixedPort struct {
	// Published is the host port (or range) as Compose reports it.
	Published string
	// Target is the container port it maps to; 0 when Compose reports none.
	Target int
}

// FixedPublishedPorts are the host ports the consumer's Compose file
// publishes itself, by service, in the file's own order: ports with a
// `published` value, which berth neither allocates nor manages. A port
// with no host port (Docker picks one) is not fixed and is left out. Read
// from the consumer's file alone, so berth's own loopback publication is
// never among them. `doctor` warns when a routed service has any.
func (c *Compose) FixedPublishedPorts(ctx context.Context, ref worktree.Ref) (map[string][]FixedPort, error) {
	services, _, _, err := c.readOrderedModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	fixed := map[string][]FixedPort{}
	for _, name := range services.keys {
		service, _ := services.values[name].(*jsonKV)
		entries, _ := service.values["ports"].([]any)
		for _, item := range entries {
			entry, _ := item.(*jsonKV)
			published, _ := entry.get("published")
			text := stringifyJSONValue(published)
			if published == nil || text == "" || text == "0" {
				continue
			}
			target, _ := entry.get("target")
			port, _ := numberToInt(target)
			fixed[name] = append(fixed[name], FixedPort{Published: text, Target: port})
		}
	}
	return fixed, nil
}

// CheckPublishable refuses a routed service berth cannot publish a loopback
// port for (network_mode host or service:…, several replicas), reading only
// the consumer's Compose file. `up` calls it before allocating, so no port
// is ever recorded for a service that cannot have one.
func (c *Compose) CheckPublishable(ctx context.Context, ref worktree.Ref) error {
	if len(c.options.Routes) == 0 {
		return nil
	}
	services, _, _, err := c.readOrderedModel(ctx, ref)
	if err != nil {
		return err
	}
	for _, route := range c.options.Routes {
		// A route naming no such service is prepare's refusal, with its own words.
		if service, ok := services.values[route.Service].(*jsonKV); ok {
			if err := refuseUnpublishable(route.Service, c.options.ComposeFile, service); err != nil {
				return err
			}
		}
	}
	return nil
}
