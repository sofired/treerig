package worktreestate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// DefaultInventoryTimeout bounds one `docker ps` call: 30s, the same bound
// doctor uses for the same kind of introspection call, and the same 30s
// every other introspection docker call this codebase makes
// (internal/backend/compose.go's own timeouts comment).
const DefaultInventoryTimeout = 30 * time.Second

// Container is one managed container, as `docker ps -a` reports it.
type Container struct {
	// ID is the container's ID, for the inspect a 137 exit needs.
	ID           string
	RepositoryID string
	// WorktreeID is empty for a container of a repository's shared services,
	// which belong to no worktree (Shared is then true).
	WorktreeID string
	Service    string
	// Shared marks a container of a repository's shared services.
	Shared bool
	// State and ExitCode are the container's service state; ExitCode is how
	// an Exited one ended. Detail is Docker's own word for an Unknown state.
	State    worktree.ServiceState
	ExitCode int
	Detail   string
}

// Available means running, and not reporting itself unhealthy or still
// starting.
func (c Container) Available() bool { return c.State == worktree.Running }

// Status is the container as one of a worktree's services.
func (c Container) Status() worktree.ServiceStatus {
	return worktree.ServiceStatus{Service: c.Service, State: c.State, ExitCode: c.ExitCode, Detail: c.Detail}
}

// healthPattern matches the health a `docker ps` status announces in
// parentheses, case-insensitively, wherever it appears in the status text:
// `Up 2 minutes (healthy)`, `Up 5 seconds (health: starting)`, `Up 1 hour
// (unhealthy)`.
var healthPattern = regexp.MustCompile(`(?i)\((healthy|unhealthy|health: starting)\)`)

// exitPattern matches how `docker ps` words an exited container: `Exited (3)
// 5 minutes ago`.
var exitPattern = regexp.MustCompile(`(?i)^Exited \((-?\d+)\)`)

// ParseInventory parses the tab-separated `docker ps` format the inventory
// asks for: state, status, repository, worktree, service, the shared label
// ("true" for a container of a repository's shared services, which carries no
// worktree label) and the container's ID. A container that exited 137 reads
// unknown here (worktree.CauseNotRead): `docker ps` cannot say whether the
// out-of-memory killer did it, and Inventory asks Docker. The state is read against a table of Docker's wordings
// (worktree.ContainerStatus): its health and exit code come from the status
// text. It never returns a nil slice on success, even for empty output, which
// means Docker answered and named no containers: Inventory.Containers uses nil
// to mean Docker could not be asked at all.
func ParseInventory(output string) ([]Container, error) {
	records := make([]Container, 0, strings.Count(output, "\n")+1)
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return nil, clierr.New("Docker returned an unrecognized container inventory line.")
		}
		state, status, repositoryID, worktreeID, service, sharedLabel, id := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6]
		isShared := sharedLabel == "true"
		// The shared field is exactly "true" or empty: anything else is a
		// tab inside an earlier field that shifted the rest.
		if state == "" || repositoryID == "" || service == "" || id == "" || (sharedLabel != "" && !isShared) || (worktreeID == "") != isShared {
			return nil, clierr.New("Docker returned an unrecognized container inventory line.")
		}
		health := ""
		if found := healthPattern.FindStringSubmatch(status); found != nil {
			health = strings.TrimPrefix(strings.ToLower(found[1]), "health: ")
		}
		code, known := 0, false
		if found := exitPattern.FindStringSubmatch(status); found != nil {
			if parsed, err := strconv.Atoi(found[1]); err == nil {
				code, known = parsed, true
			}
		}
		reading := worktree.ContainerStatus(state, health, code, known, false, false)
		records = append(records, Container{
			ID: id, RepositoryID: repositoryID, WorktreeID: worktreeID, Service: service, Shared: isShared,
			State: reading.State, ExitCode: reading.ExitCode, Detail: text.Printable(reading.Detail),
		})
	}
	return records, nil
}

// Inventory is what Docker said about the managed containers of a set of
// label namespaces.
type Inventory struct {
	// Answered is whether Docker could be asked; Containers is nil when not.
	Answered   bool
	Containers []Container
	// Error says why it could not be: the page and the tables are read by
	// people who did not run the command.
	Error string
}

// formatInventory is the `docker ps --format` the inventory asks one label
// namespace with.
func formatInventory(namespace string) string {
	return strings.Join([]string{
		"{{.State}}", "{{.Status}}",
		fmt.Sprintf(`{{.Label "%s.repository-uuid"}}`, namespace),
		fmt.Sprintf(`{{.Label "%s.worktree-uuid"}}`, namespace),
		`{{.Label "com.docker.compose.service"}}`,
		// Present only on a container of a repository's shared services,
		// which carries no worktree label.
		fmt.Sprintf(`{{.Label "%s.shared"}}`, namespace),
		"{{.ID}}",
	}, "\t")
}

// inventoryOf runs `docker ps -a` for one label namespace: the containers it
// named, the reason it could not answer (Docker unavailable, a non-zero exit,
// a timeout, or an inventory line it could not parse), or a genuine error that
// must abort the whole gathering rather than read as "Docker could not be
// asked": a cancellation, most pointedly, which is let through unchanged
// rather than folded into its string return.
func (g *Gatherer) inventoryOf(ctx context.Context, namespace string) (records []Container, reason string, err error) {
	result, err := docker.Run(ctx, g.runner, []string{
		"ps", "--all", "--filter", "label=" + namespace + ".managed=true", "--format", formatInventory(namespace),
	}, run.Spec{AllowFailure: true, Timeout: g.inventoryTimeout})
	if err != nil {
		var unavailable *docker.UnavailableError
		if errors.As(err, &unavailable) {
			return nil, unavailable.Error(), nil
		}
		return nil, "", err
	}
	if result.TimedOut {
		return nil, fmt.Sprintf("`docker ps` didn't answer within %ds", int(g.inventoryTimeout.Seconds())), nil
	}
	if result.ExitCode != 0 {
		return nil, docker.Said(result.Stderr, fmt.Sprintf("`docker ps` exited %d without saying why", result.ExitCode)), nil
	}
	records, parseErr := ParseInventory(result.Stdout)
	if parseErr != nil {
		return nil, parseErr.Error(), nil
	}
	return records, "", nil
}

// answer is one namespace's inventoryOf answer, kept at its original index so
// Inventory can merge them back in namespace order however the goroutines that
// fetched them finished.
type answer struct {
	records []Container
	reason  string
}

// Inventory asks Docker about every namespace concurrently, one `docker ps -a`
// each, and returns the merged inventory, or the first reason, in namespace
// order, that any could not answer: a worktree's state cannot be told from a
// partial inventory. errgroup.WithContext cancels every other namespace's
// still-running `docker ps` the moment one returns a real error (Docker
// unavailable is not one: it is turned into a reason, not an error), so a
// genuine failure, most pointedly the caller's own context being cancelled,
// aborts the whole call promptly rather than waiting out every remaining
// namespace's own timeout.
func (g *Gatherer) Inventory(ctx context.Context, namespaces []string) (Inventory, error) {
	answers := make([]answer, len(namespaces))
	group, groupCtx := errgroup.WithContext(ctx)
	for i, namespace := range namespaces {
		group.Go(func() error {
			records, reason, err := g.inventoryOf(groupCtx, namespace)
			if err != nil {
				return err
			}
			answers[i] = answer{records: records, reason: reason}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return Inventory{}, err
	}
	for _, a := range answers {
		if a.reason != "" {
			return Inventory{Error: a.reason}, nil
		}
	}
	// Non-nil from the start: Containers nil means Docker could not be asked,
	// and appending zero records from an empty namespace's answer must not
	// silently turn a successful, empty inventory back into that.
	all := []Container{}
	for _, a := range answers {
		all = append(all, a.records...)
	}
	if err := g.readKills(ctx, all); err != nil {
		return Inventory{}, err
	}
	return Inventory{Answered: true, Containers: all}, nil
}

// inspectConcurrency bounds how many containers readKills inspects at once.
const inspectConcurrency = 8

// readKills asks Docker about each container that exited 137, the one exit
// `docker ps` cannot explain: a stop (`compose stop`, a reboot) and the
// out-of-memory killer both leave it. The inspects run concurrently under one
// shared deadline (the inventory timeout), so many such containers cost no more
// than one. A container that cannot be inspected stays unknown: nothing assumes
// it was a stop. Only a cancellation is an error.
func (g *Gatherer) readKills(ctx context.Context, containers []Container) error {
	deadline, cancel := context.WithTimeout(ctx, g.inventoryTimeout)
	defer cancel()
	var group errgroup.Group
	group.SetLimit(inspectConcurrency)
	for i := range containers {
		container := &containers[i]
		if container.State != worktree.Unknown || container.Detail != worktree.CauseNotRead {
			continue
		}
		group.Go(func() error {
			inspected, found, err := docker.InspectContainer(deadline, g.runner, container.ID, g.inventoryTimeout)
			if err == nil && found {
				reading := worktree.ContainerStatus("exited", "", inspected.ExitCode, true, inspected.OOMKilled, true)
				container.State, container.ExitCode, container.Detail = reading.State, reading.ExitCode, text.Printable(reading.Detail)
			}
			return nil
		})
	}
	// The workers return nil, whatever the inspect said (an unreadable
	// container stays unknown), so there is no error to take from Wait.
	_ = group.Wait()
	return ctx.Err()
}
