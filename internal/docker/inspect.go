package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/run"
)

// Container is the part of `docker container inspect` berth reads. It is
// decoded from the JSON document rather than a --format template, so one
// inspect answers everything and the shape is Docker's documented one.
type Container struct {
	// ID is the full container ID.
	ID string
	// Name is the container's name, without Docker's leading slash.
	Name string
	// Status is Docker's state: created, running, paused, restarting,
	// removing, exited or dead.
	Status string
	// Health is "starting", "healthy" or "unhealthy" for a container with a
	// health check, and "" for one without.
	Health string
	// Labels are the container's labels.
	Labels map[string]string
	// Networks are the networks the container is attached to, by name.
	Networks map[string]Endpoint
	// Ports are the published ports, keyed like "1025/tcp".
	Ports map[string][]PortBinding
	// Volumes are the names of the volumes the container mounts.
	Volumes []string
	// MountPaths maps each of those volumes to where the container mounts it.
	MountPaths map[string]string
}

// Endpoint is a container's attachment to one network.
type Endpoint struct {
	// NetworkID is the network's ID.
	NetworkID string
	// Aliases are the names the container answers to on the network.
	Aliases []string
}

// PortBinding is one published port.
type PortBinding struct {
	HostIP   string
	HostPort string
}

// Network is the part of `docker network inspect` berth reads.
type Network struct {
	ID     string
	Name   string
	Labels map[string]string
	// Containers are the IDs of the containers attached to it.
	Containers []string
}

type containerDocument struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID string   `json:"NetworkID"`
			Aliases   []string `json:"Aliases"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

type networkDocument struct {
	ID         string                     `json:"Id"`
	Name       string                     `json:"Name"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]json.RawMessage `json:"Containers"`
}

// noSuchObject matches what `docker inspect` says about a resource that is
// not there: "No such object", "No such container", "No such network" or, for
// a network, "network x not found". Matched on the message because the exit
// code is 1 for every failure.
var noSuchObject = regexp.MustCompile(`(?i)no such (object|container|network|volume)|network \S+ not found`)

// NoSuchObject reports whether stderr says the resource asked about is not there.
func NoSuchObject(stderr string) bool { return noSuchObject.MatchString(stderr) }

// InspectContainer reads one container. It reports false, not an error, for a
// container that does not exist; any other failure is an error, so a caller
// never mistakes "could not tell" for "absent".
func InspectContainer(ctx context.Context, runner run.Runner, id string, timeout time.Duration) (Container, bool, error) {
	result, err := Run(ctx, runner, []string{"container", "inspect", id}, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		return Container{}, false, err
	}
	if result.ExitCode != 0 {
		if NoSuchObject(result.Stderr) {
			return Container{}, false, nil
		}
		return Container{}, false, inspectFailure("container", id, result)
	}
	var documents []containerDocument
	if err := json.Unmarshal([]byte(result.Stdout), &documents); err != nil || len(documents) != 1 {
		return Container{}, false, clierr.Newf("Docker returned an unreadable description of container %s.", id)
	}
	document := documents[0]
	container := Container{
		ID:       document.ID,
		Name:     strings.TrimPrefix(document.Name, "/"),
		Status:   document.State.Status,
		Labels:   document.Config.Labels,
		Networks: map[string]Endpoint{},
		Ports:    map[string][]PortBinding{},
	}
	if document.State.Health != nil {
		container.Health = document.State.Health.Status
	}
	for name, endpoint := range document.NetworkSettings.Networks {
		container.Networks[name] = Endpoint{NetworkID: endpoint.NetworkID, Aliases: endpoint.Aliases}
	}
	for port, bindings := range document.NetworkSettings.Ports {
		for _, binding := range bindings {
			container.Ports[port] = append(container.Ports[port], PortBinding{HostIP: binding.HostIP, HostPort: binding.HostPort})
		}
	}
	for _, mount := range document.Mounts {
		if mount.Type == "volume" && mount.Name != "" {
			container.Volumes = append(container.Volumes, mount.Name)
			if container.MountPaths == nil {
				container.MountPaths = map[string]string{}
			}
			container.MountPaths[mount.Name] = mount.Destination
		}
	}
	if container.Labels == nil {
		container.Labels = map[string]string{}
	}
	return container, true, nil
}

// Volume is the part of `docker volume inspect` berth reads.
type Volume struct {
	Name   string
	Labels map[string]string
}

// InspectVolume reads one volume. It reports false, not an error, for a
// volume that does not exist.
func InspectVolume(ctx context.Context, runner run.Runner, name string, timeout time.Duration) (Volume, bool, error) {
	result, err := Run(ctx, runner, []string{"volume", "inspect", name}, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		return Volume{}, false, err
	}
	if result.ExitCode != 0 {
		if NoSuchObject(result.Stderr) {
			return Volume{}, false, nil
		}
		return Volume{}, false, inspectFailure("volume", name, result)
	}
	var documents []struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &documents); err != nil || len(documents) != 1 {
		return Volume{}, false, clierr.Newf("Docker returned an unreadable description of volume %s.", name)
	}
	volume := Volume{Name: documents[0].Name, Labels: documents[0].Labels}
	if volume.Labels == nil {
		volume.Labels = map[string]string{}
	}
	return volume, true, nil
}

// InspectNetwork reads one network by ID or name. It reports false, not an
// error, for a network that does not exist.
func InspectNetwork(ctx context.Context, runner run.Runner, id string, timeout time.Duration) (Network, bool, error) {
	result, err := Run(ctx, runner, []string{"network", "inspect", id}, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		return Network{}, false, err
	}
	if result.ExitCode != 0 {
		if NoSuchObject(result.Stderr) {
			return Network{}, false, nil
		}
		return Network{}, false, inspectFailure("network", id, result)
	}
	var documents []networkDocument
	if err := json.Unmarshal([]byte(result.Stdout), &documents); err != nil || len(documents) != 1 {
		return Network{}, false, clierr.Newf("Docker returned an unreadable description of network %s.", id)
	}
	document := documents[0]
	network := Network{ID: document.ID, Name: document.Name, Labels: document.Labels}
	if network.Labels == nil {
		network.Labels = map[string]string{}
	}
	for container := range document.Containers {
		network.Containers = append(network.Containers, container)
	}
	return network, true, nil
}

// VerifyConsumer reports whether container is a running container of one of
// repositoryID's worktrees that uses the shared service called shared, by its
// labels alone (managed, the repository's, a worktree's, shared-use, and not
// itself a shared container), and the worktree it belongs to. It is the one
// proof both the sweep's lookup by repository and a worktree's own lookup
// make.
func VerifyConsumer(container Container, namespace, repositoryID, shared string) (worktreeID string, ok bool) {
	worktreeID = container.Labels[brand.WorktreeLabel(namespace)]
	ok = container.Status == "running" && worktreeID != "" &&
		container.Labels[brand.ManagedLabel(namespace)] == "true" &&
		container.Labels[brand.RepositoryLabel(namespace)] == repositoryID &&
		container.Labels[brand.SharedUseLabel(namespace, shared)] == "true" &&
		container.Labels[brand.SharedLabel(namespace)] != "true"
	return worktreeID, ok
}

// inspectFailure is the error for an inspect that failed for a reason other
// than the resource being absent.
func inspectFailure(kind, id string, result run.Result) error {
	return clierr.Newf("Could not inspect %s %s: %s", kind, id, Said(result.Stderr, fmt.Sprintf("docker exited %d", result.ExitCode)))
}
