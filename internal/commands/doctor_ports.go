package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// loopbackPortChecks are `doctor`'s lines about a registered worktree's
// loopback ports (#119): for each routed service, whether its allocated port
// is free or held by this worktree's own container, and a warning for a
// routed service whose own Compose file also publishes a fixed host port.
// Ports shared between worktrees are reported by registryChecks, since they
// span the whole registry. Nothing here writes.
func loopbackPortChecks(ctx context.Context, opts doctorOptions, loaded *config.Loaded, registration *registry.WorktreeRegistration) ([]Check, error) {
	ref := worktree.Ref{RepositoryID: registration.RepositoryID, WorktreeID: registration.WorktreeID, Slug: registration.Slug}
	runtime := backend.NewCompose(backend.Options{
		ComposeFile: composeFilePath(loaded), ProjectRoot: loaded.Root, ConfigFile: loaded.File,
		ProjectName: loaded.Config.Project.Name, Domain: loaded.Config.Project.Domain,
		ControlHome: opts.ControlHome, LabelNamespace: loaded.Config.LabelNamespace(),
		Runner: opts.Runner, Logger: opts.Logger, Environ: opts.Environ,
	})
	probe := opts.PortProbe
	if probe == nil {
		probe = ports.LoopbackProbe{}
	}

	var checks []Check
	for _, service := range routedServiceNames(loaded.Config.Service) {
		check, err := portCheck(ctx, opts, probe, runtime, ref, registration, service)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}

	published, err := guarded("checkout.published-port-scan", "published ports", func() ([]Check, error) {
		if !opts.DockerAnswered {
			return nil, nil
		}
		fixed, err := runtime.FixedPublishedPorts(ctx, ref)
		if err != nil {
			return nil, err
		}
		var lines []Check
		for _, service := range routedServiceNames(loaded.Config.Service) {
			if len(fixed[service]) == 0 {
				continue
			}
			mappings := make([]string, len(fixed[service]))
			for i, port := range fixed[service] {
				mappings[i] = fmt.Sprintf("%s to container port %d", port.Published, port.Target)
			}
			lines = append(lines, Check{Status: StatusWarn, ID: "checkout.published-port", Instance: service,
				Subject: service + " published port", Detail: fmt.Sprintf(
					"its Compose file publishes host port %s itself. %s already publishes %s on its own loopback port, so drop the fixed mapping: "+
						"it can collide with another worktree's copy of this stack.",
					text.OneLine(strings.Join(mappings, ", "), 120), brand.Name, service)})
		}
		return lines, nil
	})
	if err != nil {
		return nil, err
	}
	return append(checks, published...), nil
}

// portCheck is one routed service's `checkout.ports` line: skipped before a
// port exists, ok when it is free or this worktree's own container holds it,
// a failure when anything else does, and a warning when it is busy and Docker
// cannot say by what, or when the service's running container publishes a
// different loopback port than the one recorded.
func portCheck(ctx context.Context, opts doctorOptions, probe ports.Probe, runtime *backend.Compose, ref worktree.Ref, registration *registry.WorktreeRegistration, service string) (Check, error) {
	subject := service + " port"
	port, allocated := registration.Ports[service]
	if !allocated {
		return Check{Status: StatusSkip, ID: "checkout.ports", Instance: service, Subject: subject,
			Detail: fmt.Sprintf("none allocated yet; '%s up' allocates one", opts.CommandName)}, nil
	}
	check, err := heldPortCheck(ctx, opts, probe, runtime, ref, port, service, subject)
	if err != nil || check.Status != StatusOK || !opts.DockerAnswered {
		return check, err
	}
	// The port is free or this worktree's own: is the running container on it?
	publishers, err := runtime.ServicePublishers(ctx, ref, service)
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) || ctx.Err() != nil {
			return Check{}, err
		}
		return check, nil
	}
	if len(publishers) == 0 {
		return check, nil
	}
	var actual []string
	for _, publisher := range publishers {
		if slices.Contains(publisher.LoopbackPorts, port) {
			return check, nil
		}
		for _, other := range publisher.LoopbackPorts {
			actual = append(actual, fmt.Sprintf("127.0.0.1:%d", other))
		}
	}
	published := "no loopback port"
	if len(actual) > 0 {
		published = strings.Join(actual, ", ")
	}
	return Check{Status: StatusWarn, ID: "checkout.ports", Instance: service, Subject: subject, Detail: fmt.Sprintf(
		"127.0.0.1:%d is recorded, but the running container publishes %s. Run '%s up' to recreate it on the recorded port.",
		port, published, opts.CommandName)}, nil
}

// heldPortCheck is portCheck's answer about who holds an allocated port.
func heldPortCheck(ctx context.Context, opts doctorOptions, probe ports.Probe, runtime *backend.Compose, ref worktree.Ref, port int, service, subject string) (Check, error) {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	if probe.Free(ctx, port) {
		return Check{Status: StatusOK, ID: "checkout.ports", Instance: service, Subject: subject, Detail: address + " is free"}, nil
	}
	if !opts.DockerAnswered {
		return Check{Status: StatusWarn, ID: "checkout.ports", Instance: service, Subject: subject,
			Detail: address + " is in use, and Docker is not answering, so what holds it is unknown"}, nil
	}
	state, err := inspectPort(ctx, probe, runtime, ref, port, false, func() []registry.PortClaim {
		return otherClaims(opts.ControlHome, ref, service, port)
	})
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) || ctx.Err() != nil {
			return Check{}, err
		}
		return Check{Status: StatusWarn, ID: "checkout.ports", Instance: service, Subject: subject,
			Detail: fmt.Sprintf("%s is in use; asking Docker what holds it failed: %s", address, text.OneLine(err.Error(), 200))}, nil
	}
	switch {
	case state.Free:
		return Check{Status: StatusOK, ID: "checkout.ports", Instance: service, Subject: subject, Detail: address + " is free"}, nil
	case len(state.Others) == 0:
		names := make([]string, len(state.Own))
		for i, publisher := range state.Own {
			names[i] = publisher.Name
		}
		return Check{Status: StatusOK, ID: "checkout.ports", Instance: service, Subject: subject,
			Detail: fmt.Sprintf("%s is held by this worktree's own container %s", address, strings.Join(names, ", "))}, nil
	default:
		return Check{Status: StatusFail, ID: "checkout.ports", Instance: service, Subject: subject, Detail: fmt.Sprintf(
			"%s is held by %s. Stop it, or run '%s up --reallocate-port %s' to move %s to another port.",
			address, text.OneLine(describeHolders(opts.OS, port, state.Others), 300), opts.CommandName, service, service)}, nil
	}
}
