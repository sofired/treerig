// Package worktreestate gathers what worktree.Classify decides a worktree's
// state from, and is the one place that asks: `status`, `worktree list`, the
// overview page and `doctor` all call it, so a worktree has the same state
// word everywhere (docs/design/cli-1.0.md, "State words").
//
// Across worktrees it reads each registration's history (ready or pending, and
// the removal marker), asks Git which worktrees the current repository has and
// what became of their directories, and asks Docker once per label namespace
// (`docker ps -a`). For one worktree the caller passes the services
// `docker compose ps --all --format json` reported, which carry exit codes.
// The package depends on the registry and on worktree, never the reverse, so
// worktree stays a leaf.
package worktreestate

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// Options configure a Gatherer.
type Options struct {
	// ControlHome is where the registrations and their removal markers live.
	ControlHome string
	Runner      run.Runner
	// InventoryTimeout bounds one `docker ps`; zero uses DefaultInventoryTimeout.
	InventoryTimeout time.Duration
	// Mounts reads the mount table; nil reads the machine's. A test supplies its own.
	Mounts func() (Mounts, error)
}

// Gatherer gathers the inputs of Classify. It is made for one run: it reads
// the mount table at most once.
type Gatherer struct {
	controlHome      string
	runner           run.Runner
	inventoryTimeout time.Duration
	readMounts       func() (Mounts, error)

	mountsOnce sync.Once
	mounts     Mounts
	mountsErr  error
}

// New builds a Gatherer from options.
func New(options Options) *Gatherer {
	g := &Gatherer{
		controlHome: options.ControlHome, runner: options.Runner,
		inventoryTimeout: options.InventoryTimeout, readMounts: options.Mounts,
	}
	if g.inventoryTimeout <= 0 {
		g.inventoryTimeout = DefaultInventoryTimeout
	}
	if g.readMounts == nil {
		g.readMounts = readMounts
	}
	return g
}

// Key is the map key a worktree's Result is found under: repositoryId:worktreeId.
func Key(repositoryID, worktreeID string) string { return repositoryID + ":" + worktreeID }

// Basis is how a directory verdict was reached, so a caller that acts on one
// (a removal of every worktree whose directory is gone, #211) can demand the
// stronger.
type Basis int

const (
	// BasisNone is no directory verdict: the state came from elsewhere, or
	// nobody asked Git about this worktree.
	BasisNone Basis = iota
	// BasisUUID is a verdict from the Git entry that carries the worktree's
	// own identity.
	BasisUUID
	// BasisRecordedRoot is a verdict from the directory the last `up` recorded
	// alone: Git lists no entry with the worktree's identity.
	BasisRecordedRoot
)

// Result is a worktree's state, with the cause when it is unknown.
type Result struct {
	State worktree.State
	// Hint names the cause of an unknown state.
	Hint string
	// Basis is how the directory was judged, whatever the state came out as.
	Basis Basis
}

// Repository is what the Gatherer asks Git of a repository;
// *registry.IdentityService answers it, and Answered replays an answer a
// caller already has.
type Repository interface {
	Entries(ctx context.Context) ([]registry.WorktreeEntry, error)
}

// Answered is the answer a caller already has from Git, handed to the
// Gatherer as the Repository it asks, so Git is asked once.
type Answered struct {
	Listed []registry.WorktreeEntry
	Err    error
}

// Entries answers what Git was asked.
func (a Answered) Entries(context.Context) ([]registry.WorktreeEntry, error) {
	return a.Listed, a.Err
}

// Subject is one registration to classify, with what the caller knows of it
// that the registration does not hold (SubjectOf builds one from its card).
type Subject struct {
	Registration registry.RegisteredWorktree
	// Declared are the names of the services the last `up` declared (the
	// index card): one with no container is not created, not absent from the
	// picture.
	Declared []string
	// RecordedRoot is the directory the last `up` saw the worktree in.
	RecordedRoot string
	// Unreadable says why the card could not be read, when it could not: the
	// state is then unknown, never guessed from containers alone.
	Unreadable string
}

// Request is what Gather classifies.
type Request struct {
	Subjects []Subject
	// Namespaces are the label namespaces to ask Docker about, one `docker ps`
	// each.
	Namespaces []string
	// Repositories are the repositories Git is asked about, by repository ID:
	// the one a command runs in today. A worktree of any other has no
	// directory evidence.
	Repositories map[string]Repository
}

// Report is what Gather found.
type Report struct {
	// Docker is the inventory every state was told from; the overview reads
	// its containers for the services' links.
	Docker Inventory
	// GitError says why Git could not be asked about a repository, when it
	// could not (the first, if several).
	GitError string
	// GitHint is the unknown-state hint every worktree of that repository
	// carries then, so a caller can print the cause once and not per row.
	GitHint string
	// States is each subject's state, by Key.
	States map[string]Result
}

// answer is what Git said about one repository.
type gitAnswer struct {
	entries []registry.WorktreeEntry
	err     error
}

// DockerHint words the reason Docker could not be asked as a sentence a
// worktree's unknown state gives.
func DockerHint(reason string) string {
	if reason == "" {
		return "Docker didn't answer."
	}
	if strings.HasPrefix(reason, "Docker") || strings.HasPrefix(reason, "`docker") {
		return reason
	}
	return "Docker didn't answer: " + reason
}

// Gather classifies every subject. Docker or Git not answering is not an error:
// it makes the states they decide unknown. Only a failure that must stop the
// command (a cancellation) is returned.
func (g *Gatherer) Gather(ctx context.Context, request Request) (Report, error) {
	inventory, err := g.Inventory(ctx, request.Namespaces)
	if err != nil {
		return Report{}, err
	}
	report := Report{Docker: inventory, States: make(map[string]Result, len(request.Subjects))}

	git := make(map[string]gitAnswer, len(request.Repositories))
	for id, repository := range request.Repositories {
		entries, gitErr := repository.Entries(ctx)
		if gitErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Report{}, ctxErr
			}
			if report.GitError == "" {
				report.GitError = gitErr.Error()
				report.GitHint = couldNotCheck(gitErr)
			}
		}
		git[id] = gitAnswer{entries: entries, err: gitErr}
	}

	owned := map[string][]Container{}
	for _, container := range inventory.Containers {
		if !container.Shared {
			key := Key(container.RepositoryID, container.WorktreeID)
			owned[key] = append(owned[key], container)
		}
	}

	for _, subject := range request.Subjects {
		record := subject.Registration
		key := Key(record.RepositoryID, record.WorktreeID)
		in := g.history(subject)
		var basis Basis
		if answered, asked := git[record.RepositoryID]; asked {
			var found evidence
			if answered.err != nil {
				found = g.directoryOf(nil, nil, subject.RecordedRoot, record.Slug, answered.err)
			} else {
				entry, matched := entryFor(answered.entries, record.WorktreeID, subject.RecordedRoot)
				found = g.directoryOf(entry, answered.entries, subject.RecordedRoot, record.Slug, nil)
				if entry != nil {
					basis = matched
				} else {
					basis = BasisRecordedRoot
				}
			}
			in.Directory, in.DirectoryHint = found.directory, found.hint
		}
		if in.RecordError == "" && subject.Unreadable != "" {
			in.RecordError = subject.Unreadable
		}
		if inventory.Answered {
			in.Services = servicesOf(subject.Declared, owned[key])
		} else {
			in.ServicesHint = DockerHint(inventory.Error)
		}
		classified := worktree.Classify(in)
		report.States[key] = Result{State: classified.State, Hint: classified.Hint, Basis: basis}
	}
	return report, nil
}

// Current classifies the worktree a command runs in. directory is what the
// caller knows of it (a command run from inside the worktree passes
// DirectoryPresent); services are what `docker compose ps --all --format
// json` reported for it; servicesHint, when it is not empty, says why they
// could not be read, worded by the caller, which knows whether Docker or
// Compose failed.
func (g *Gatherer) Current(record registry.RegisteredWorktree, services []worktree.ServiceStatus, directory worktree.Directory, servicesHint string) Result {
	in := g.history(Subject{Registration: record})
	in.Directory = directory
	if servicesHint != "" {
		in.ServicesHint = text.OneLine(servicesHint, 300)
	} else {
		in.Services = services
	}
	classified := worktree.Classify(in)
	return Result{State: classified.State, Hint: classified.Hint}
}

// history is the part of Classify's inputs the registration holds: whether an
// `up` ever finished and whether a removal was started. A record that cannot
// be read is RecordError, never a guess.
func (g *Gatherer) history(subject Subject) worktree.Inputs {
	record := subject.Registration
	store := registry.NewStore(g.controlHome, record.RepositoryID)
	var in worktree.Inputs
	initialization, err := store.Initialization(record.WorktreeID, record.Slug)
	if err != nil {
		in.RecordError = text.OneLine(err.Error(), 300)
	}
	in.Started = initialization == registry.Complete
	removing, err := store.Removing(record.WorktreeID)
	if err != nil && in.RecordError == "" {
		in.RecordError = text.OneLine(err.Error(), 300)
	}
	in.Removing = removing
	return in
}

// entryFor is the entry of the repository's worktree list that is this
// worktree: the one carrying its identity (BasisUUID), or failing that the one
// at the directory the last `up` saw it in, if that carries no identity of its
// own (BasisRecordedRoot; another worktree now living there is not this one).
func entryFor(entries []registry.WorktreeEntry, worktreeID, recordedRoot string) (*registry.WorktreeEntry, Basis) {
	for i := range entries {
		if entries[i].WorktreeID == worktreeID {
			return &entries[i], BasisUUID
		}
	}
	if recordedRoot != "" {
		for i := range entries {
			if entries[i].Root == recordedRoot && entries[i].WorktreeID == "" {
				return &entries[i], BasisRecordedRoot
			}
		}
	}
	return nil, BasisNone
}

// servicesOf is a worktree's containers, one entry each, and a not-created
// entry for each declared service that has none.
func servicesOf(declared []string, containers []Container) []worktree.ServiceStatus {
	services := make([]worktree.ServiceStatus, 0, len(declared)+len(containers))
	have := map[string]bool{}
	for _, container := range containers {
		services = append(services, container.Status())
		have[container.Service] = true
	}
	for _, name := range declared {
		if !have[name] {
			services = append(services, worktree.ServiceStatus{Service: name, State: worktree.NotCreated})
			have[name] = true
		}
	}
	return services
}
