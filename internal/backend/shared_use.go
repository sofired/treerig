package backend

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/worktree"
)

// SharedUseSpec is one shared service as the worktree's runtime needs to
// check it: its name and the worktree services that use it.
type SharedUseSpec struct {
	Name   string
	UsedBy []string
}

// CheckSharedUse refuses a worktree whose Compose file cannot take the
// shared services it is configured to use (docs/design/routing.md, decisions
// 15 and 22): a shared name that is also a service of the file, which would
// make the alias resolve to two things; a used-by service the file does not
// define; and an active used-by service with network_mode host, none,
// service: or container:, which cannot join a network. Inactive profiles are
// checked for name collisions and missing services only. It reads the
// consumer's file only and returns errors from reading its Compose model.
func (c *Compose) CheckSharedUse(ctx context.Context, ref worktree.Ref, shared []SharedUseSpec) error {
	if len(shared) == 0 {
		return nil
	}
	services, _, _, err := c.readOrderedModel(ctx, ref)
	if err != nil {
		return err
	}
	file := c.options.ComposeFile
	// A service behind an inactive profile is defined, only not in this
	// model: it is checked against the file with every profile on before it
	// is called missing.
	var everything *jsonKV
	defined := func(name string) (bool, error) {
		if services.has(name) {
			return true, nil
		}
		if everything == nil {
			all, err := c.servicesInAllProfiles(ctx, ref)
			if err != nil {
				return false, err
			}
			everything = all
		}
		return everything.has(name), nil
	}
	for _, spec := range shared {
		ok, err := defined(spec.Name)
		if err != nil {
			return err
		}
		if ok {
			return clierr.Newf(
				"[shared.service.%s] has the same name as a service in %s. A shared service is reached by its name on the worktree's network, so the name would mean two things: rename one of them.",
				spec.Name, file)
		}
		for _, use := range spec.UsedBy {
			ok, err := defined(use)
			if err != nil {
				return err
			}
			if !ok {
				return clierr.Newf("[shared.service.%s] lists %s in used-by, which is not defined in %s.", spec.Name, use, file)
			}
			service, present := services.values[use].(*jsonKV)
			if !present {
				continue
			}
			if mode, ok := service.get("network_mode"); ok {
				if text, ok := mode.(string); ok && cannotJoinNetwork(text) {
					return clierr.Newf(
						"Service %s in %s uses network_mode: %s, so the shared service %s cannot be connected to its network. "+
							"Remove network_mode, or remove %s from used-by in [shared.service.%s].",
						use, file, text, spec.Name, use, spec.Name)
				}
			}
		}
	}
	return nil
}

// cannotJoinNetwork reports whether a network_mode puts a service where a
// shared container cannot be attached beside it: the host's namespace,
// none, or another container's or service's.
func cannotJoinNetwork(mode string) bool {
	return mode == "host" || mode == "none" || strings.HasPrefix(mode, "service:") || strings.HasPrefix(mode, "container:")
}

// servicesInAllProfiles is the consumer's services with every profile enabled.
func (c *Compose) servicesInAllProfiles(ctx context.Context, ref worktree.Ref) (*jsonKV, error) {
	file := c.options.ComposeFile
	result, err := c.compose(ctx, ref, []string{"--profile", "*", "config", "--format", "json"}, []string{file},
		run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, clierr.Newf("Compose could not read %s:\n%s", file, firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)))
	}
	services, _, _, err := decodeOrderedComposeModel([]byte(result.Stdout), file)
	return services, err
}

// SharedConsumer is a running container of the worktree that uses a shared
// service, proven by the worktree's ownership labels and its shared-use label.
type SharedConsumer struct {
	// ID is the container's ID.
	ID string
	// Name is the container's name.
	Name string
	// Service is its Compose service.
	Service string
	// Networks are the networks it is attached to, by name, with their IDs.
	Networks map[string]string
}

// SharedConsumers lists the worktree's running containers that use the shared
// service called shared. Found by label and each re-checked on inspect, never
// by name; a container that vanishes between the list and the inspect is
// left out.
func (c *Compose) SharedConsumers(ctx context.Context, ref worktree.Ref, shared string) ([]SharedConsumer, error) {
	namespace := c.options.LabelNamespace
	ids, err := c.psIDs(ctx, []string{
		brand.ManagedLabel(namespace) + "=true",
		brand.RepositoryLabel(namespace) + "=" + ref.RepositoryID,
		brand.WorktreeLabel(namespace) + "=" + ref.WorktreeID,
		brand.SharedUseLabel(namespace, shared) + "=true",
	})
	if err != nil {
		return nil, err
	}
	var consumers []SharedConsumer
	for _, id := range ids {
		container, found, err := docker.InspectContainer(ctx, c.options.Runner, id, c.timeouts.Probe)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if owner, ok := docker.VerifyConsumer(container, namespace, ref.RepositoryID, shared); !ok || owner != ref.WorktreeID {
			continue
		}
		networks := make(map[string]string, len(container.Networks))
		for name, endpoint := range container.Networks {
			networks[name] = endpoint.NetworkID
		}
		consumers = append(consumers, SharedConsumer{
			ID: container.ID, Name: container.Name, Service: container.Labels[ComposeServiceLabel], Networks: networks,
		})
	}
	return consumers, nil
}

// psIDs lists container IDs, running or not, matching every label filter.
func (c *Compose) psIDs(ctx context.Context, labels []string) ([]string, error) {
	return c.list(ctx, resourceTypes[0], labels)
}

// NetworkProof is a worktree network's ownership check: whether it carries
// this worktree's labels, and if not, what is wrong.
type NetworkProof struct {
	// ID is the network's ID.
	ID string
	// Name is the network's name.
	Name string
	// Owned is true when the network carries this worktree's managed,
	// repository and worktree labels and its Compose project label.
	Owned bool
	// Reason says what failed when Owned is false.
	Reason string
}

// ProveNetwork checks that the network identified by idOrName is this
// worktree's own, by its labels (managed, repository, worktree and the Compose
// project) and never by its name. A network that is gone is not owned.
func (c *Compose) ProveNetwork(ctx context.Context, ref worktree.Ref, idOrName string) (NetworkProof, error) {
	namespace := c.options.LabelNamespace
	network, found, err := docker.InspectNetwork(ctx, c.options.Runner, idOrName, c.timeouts.Probe)
	if err != nil {
		return NetworkProof{}, err
	}
	proof := NetworkProof{ID: network.ID, Name: network.Name}
	switch {
	case !found:
		proof.ID, proof.Name = idOrName, idOrName
		proof.Reason = "it no longer exists"
	case network.Labels[brand.ManagedLabel(namespace)] != "true",
		network.Labels[brand.RepositoryLabel(namespace)] != ref.RepositoryID,
		network.Labels[brand.WorktreeLabel(namespace)] != ref.WorktreeID,
		network.Labels[ComposeProjectLabel] != c.project(ref):
		proof.Reason = "its ownership labels do not match this worktree"
	default:
		// As in verify: ownership labels prove ownership only in the format
		// they were written in.
		switch verdict, found := stateformat.Label(network.Labels[brand.FormatLabel(namespace)], stateformat.DockerLabel); verdict {
		case stateformat.Current:
			proof.Owned = true
		case stateformat.FromNewer:
			proof.Reason = "it is " + stateformat.Detail(found, stateformat.DockerLabel)
		default:
			proof.Reason = stateformat.UnprovenReason(brand.FormatLabel(namespace), "it is this worktree's")
		}
	}
	return proof, nil
}

// OwnedNetworks lists the worktree's networks, found by its ownership labels
// and by its Compose project, each proven. A network that fails proof is
// returned with Owned false and its reason, for a caller to report and not
// touch. Listing and inspection errors still fail the call; networks that
// disappear before inspection are omitted.
func (c *Compose) OwnedNetworks(ctx context.Context, ref worktree.Ref) ([]NetworkProof, error) {
	namespace := c.options.LabelNamespace
	kind := resourceTypes[slices.IndexFunc(resourceTypes, func(k resourceType) bool { return k.kind == "network" })]
	byProject, err := c.list(ctx, kind, []string{ComposeProjectLabel + "=" + c.project(ref)})
	if err != nil {
		return nil, err
	}
	byOwnership, err := c.list(ctx, kind, []string{
		brand.ManagedLabel(namespace) + "=true",
		brand.RepositoryLabel(namespace) + "=" + ref.RepositoryID,
		brand.WorktreeLabel(namespace) + "=" + ref.WorktreeID,
	})
	if err != nil {
		return nil, err
	}
	ids := slices.Concat(byProject, byOwnership)
	slices.Sort(ids)
	var proofs []NetworkProof
	for _, id := range slices.Compact(ids) {
		proof, err := c.ProveNetwork(ctx, ref, id)
		if err != nil {
			return nil, err
		}
		if proof.Reason == "it no longer exists" {
			continue
		}
		proofs = append(proofs, proof)
	}
	return proofs, nil
}

// String names a network for a message.
func (p NetworkProof) String() string {
	if p.Name != "" && p.Name != p.ID {
		return fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	return p.ID
}
