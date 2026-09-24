package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// Options builds a Registry.
type Options struct {
	ControlHome string
	Root        string
	// Domain is the project domain from config; slugs are allocated unique within it.
	Domain string
	// LabelNamespace is recorded on every registration this registry writes
	// or reconciles; "" means unset. Unused by the read path — kept for the
	// write path this registry will grow.
	LabelNamespace string
	Runner         run.Runner
	Logger         logger.Logger
	// CommandName is the name the CLI was invoked as; appears in recovery hints.
	CommandName string
	// Invocation is the command line as invoked, arguments space-joined;
	// names this process to whoever waits on a lock it holds.
	Invocation string
}

// Registry is the worktree registry's read path for one checkout: identity,
// its own repository's and the whole machine's registrations.
//
// The one mutating verb, Ensure — which registers a worktree if needed,
// reconciles its record with the current config domain, and reports a
// hostname change — needs the registry lock and the worktree operation lock,
// and lives with the lock layer, not here.
type Registry struct {
	ControlHome string
	// Identities answers this checkout's Git-backed identity.
	Identities *IdentityService

	root           string
	domain         string
	labelNamespace string
	logger         logger.Logger
	commandName    string
	invocation     string
}

// New builds a Registry for one checkout.
func New(opts Options) *Registry {
	commandName := opts.CommandName
	if commandName == "" {
		commandName = brand.Name
	}
	invocation := opts.Invocation
	if invocation == "" {
		invocation = commandName
	}
	return &Registry{
		ControlHome:    layout.ControlLayout(opts.ControlHome).ControlHome,
		Identities:     NewIdentityService(opts.Root, opts.Runner),
		root:           opts.Root,
		domain:         opts.Domain,
		labelNamespace: opts.LabelNamespace,
		logger:         opts.Logger,
		commandName:    commandName,
		invocation:     invocation,
	}
}

// Store is the registrations for repositoryID, sharing this registry's control home.
func (r *Registry) Store(repositoryID string) *Store {
	return NewStore(r.ControlHome, repositoryID)
}

func registration(identity WorktreeIdentity, record RegisteredWorktree) WorktreeRegistration {
	return WorktreeRegistration{
		RegisteredWorktree: RegisteredWorktree{
			RepositoryID:   identity.RepositoryID,
			WorktreeID:     identity.WorktreeID,
			Slug:           record.Slug,
			Domain:         record.Domain,
			LabelNamespace: record.LabelNamespace,
		},
		Root:   identity.Root,
		Branch: identity.Branch,
	}
}

// Current is this checkout's own registration, read-only. Warns when the
// record disagrees with config; never repairs it. Answers (nil, nil) when
// this checkout is not registered.
func (r *Registry) Current(ctx context.Context) (*WorktreeRegistration, error) {
	identity, err := r.Identities.Inspect(ctx, r.root, InspectOptions{})
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, nil
	}
	record, err := r.Store(identity.RepositoryID).Find(identity.WorktreeID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	if record.Domain != r.domain {
		r.logger.Warn(fmt.Sprintf(
			"This worktree is registered as %s, but %s now says %s. Run '%s up' to reconcile.",
			layout.WorktreeHost(record.Slug, record.Domain), brand.ConfigFile, r.domain, r.commandName,
		))
	}
	result := registration(*identity, *record)
	return &result, nil
}

// List is this repository's own registrations.
func (r *Registry) List(ctx context.Context) ([]RegisteredWorktree, error) {
	repositoryID, present, err := r.Identities.RepositoryID(ctx, r.root)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return r.Store(repositoryID).List()
}

// ListAll is every readable record on the machine; unreadable files are
// warned about, not failed on.
func (r *Registry) ListAll(ctx context.Context) ([]RegisteredWorktree, error) {
	scan, err := ScanRegistrations(r.ControlHome)
	if err != nil {
		return nil, err
	}
	for _, problem := range scan.Problems {
		r.logger.Warn(fmt.Sprintf("Skipping unreadable registry record %s: %s", problem.File, problem.Reason))
	}
	return scan.Records, nil
}

// ScanAll is records, unreadable files, and hostname conflicts: what a
// `doctor` command reports.
func (r *Registry) ScanAll(ctx context.Context) (RegistryScan, error) {
	return ScanRegistrations(r.ControlHome)
}

// FindGlobal looks a worktree up by its UUID (unique everywhere) or its
// slug. A slug is only unique within a domain, so domain ("" means
// unrestricted) narrows the match. A slug found under several domains is an
// error asking for one; a hostname claimed by several worktrees is an error
// naming them. Never a guess.
func (r *Registry) FindGlobal(ctx context.Context, identifier, domain string) (*RegisteredWorktree, error) {
	scan, err := ScanRegistrations(r.ControlHome)
	if err != nil {
		return nil, err
	}
	for _, record := range scan.Records {
		if record.WorktreeID == identifier {
			found := record
			return &found, nil
		}
	}

	var bySlug []RegisteredWorktree
	for _, record := range scan.Records {
		if record.Slug == identifier && (domain == "" || record.Domain == domain) {
			bySlug = append(bySlug, record)
		}
	}
	if len(bySlug) == 0 {
		return nil, nil
	}
	if len(bySlug) == 1 {
		return &bySlug[0], nil
	}

	domainSet := map[string]bool{}
	var domains []string
	for _, record := range bySlug {
		if !domainSet[record.Domain] {
			domainSet[record.Domain] = true
			domains = append(domains, record.Domain)
		}
	}
	sort.Strings(domains)
	if len(domains) == 1 {
		claimants := make([]string, len(bySlug))
		for i, record := range bySlug {
			claimants[i] = record.WorktreeID
		}
		return nil, clierr.Newf(
			"Hostname %s is registered to more than one worktree (%s); address one by its UUID.",
			layout.WorktreeHost(identifier, domains[0]), strings.Join(claimants, ", "),
		)
	}
	return nil, clierr.Newf("Slug %s exists under %s; specify a domain.", identifier, strings.Join(domains, ", "))
}

// PruneCandidates are registrations whose worktree Git no longer lists, or
// whose root is gone.
func (r *Registry) PruneCandidates(ctx context.Context) ([]RegisteredWorktree, error) {
	repositoryID, present, err := r.Identities.RepositoryID(ctx, r.root)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	activeIdentities, err := r.Identities.ActiveIdentities(ctx)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, identity := range activeIdentities {
		if identity.RepositoryID == repositoryID {
			active[identity.WorktreeID] = true
		}
	}
	records, err := r.Store(repositoryID).List()
	if err != nil {
		return nil, err
	}
	var candidates []RegisteredWorktree
	for _, record := range records {
		if !active[record.WorktreeID] {
			candidates = append(candidates, record)
		}
	}
	return candidates, nil
}
