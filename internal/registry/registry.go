package registry

import (
	"context"
	"fmt"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/run"
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
	// UserHome is the caller's own resolved home directory (in production,
	// layout.HomeDir(getenv) from internal/commands' checkedHome, #80),
	// carried to every files.SecureRegistrationDirectories call Ensure
	// makes ahead of a write — this package does not read the environment
	// itself for it, only the entry point does.
	UserHome string
	// Environ is the process environment Git's refusal outside a repository
	// is asked in (Repository adds the C locale to it); nil inherits this
	// process's own.
	Environ []string
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
	userHome       string
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
	log := opts.Logger
	if log == nil {
		log = logger.Silent{}
	}
	identities := NewIdentityService(opts.Root, opts.Runner)
	identities.environ = opts.Environ
	return &Registry{
		ControlHome:    layout.ControlLayout(opts.ControlHome).ControlHome,
		Identities:     identities,
		root:           opts.Root,
		domain:         opts.Domain,
		labelNamespace: opts.LabelNamespace,
		logger:         log,
		commandName:    commandName,
		invocation:     invocation,
		userHome:       opts.UserHome,
	}
}

// Store is the registrations for repositoryID, sharing this registry's control home.
func (r *Registry) Store(repositoryID string) *Store {
	return NewStore(r.ControlHome, repositoryID)
}

// registration combines identity's own identifying fields with record's
// registered ones into the WorktreeRegistration Ensure reports.
func registration(identity WorktreeIdentity, record RegisteredWorktree) WorktreeRegistration {
	// Starts from the record, so a field the record gains is carried; the
	// identity only supplies where the worktree lives now.
	return WorktreeRegistration{RegisteredWorktree: record, Root: identity.Root, Branch: identity.Branch}
}

// Ensure registers this checkout (or reconciles its record with the current
// config domain) and reports a hostname or label-namespace change so the
// caller can re-route. A worktree whose home already exists on disk is
// adopted as complete; otherwise it is pending until initialization ends.
//
// worktree == "" means r.root, the checkout this Registry answers for.
//
// Lock order: this worktree's own operation lock is held for the whole
// call — never its development-session lock, so `dev` is unaffected — with
// the registry lock Store.Ensure takes nested inside it, on the machine-wide
// registry file, a different flock target entirely.
func (r *Registry) Ensure(ctx context.Context, worktree string) (*EnsuredRegistration, error) {
	if worktree == "" {
		worktree = r.root
	}
	identity, err := r.Identities.Ensure(ctx, worktree)
	if err != nil {
		return nil, err
	}
	if err := files.SecureRegistrationDirectories(
		layout.WorktreeLayout(r.ControlHome, identity.RepositoryID, identity.WorktreeID), r.userHome,
	); err != nil {
		return nil, err
	}

	// Named the way every later wait names it — by slug — once a record exists.
	name := identity.Root
	if known, findErr := r.Store(identity.RepositoryID).Find(identity.WorktreeID); findErr == nil && known != nil {
		name = known.Slug
	}
	markers := procid.Markers{
		Project:    layout.ComposeProjectName(identity.RepositoryID, identity.WorktreeID),
		WorktreeID: identity.WorktreeID,
	}
	operation, _ := lock.WorktreeScopes(r.ControlHome, identity.RepositoryID, identity.WorktreeID, name, markers)
	writer := Writer{Logger: r.logger, Invocation: r.invocation, UserHome: r.userHome}

	var labelNamespace *string
	if r.labelNamespace != "" {
		ns := r.labelNamespace
		labelNamespace = &ns
	}

	var ensured *EnsuredRegistration
	err = withWorktreeLock(ctx, operation, writer, func(ctx context.Context) error {
		currentIdentity, err := r.Identities.Inspect(ctx, worktree, InspectOptions{})
		if err != nil {
			return err
		}
		if currentIdentity == nil || currentIdentity.RepositoryID != identity.RepositoryID || currentIdentity.WorktreeID != identity.WorktreeID {
			return clierr.Newf("Worktree identity changed while registering %s; retry the command.", identity.Root)
		}
		record, err := r.Store(identity.RepositoryID).Ensure(
			ctx, identity.WorktreeID, r.domain,
			// The identity captured before the lock, not currentIdentity: the
			// branch a slug is derived from is the one that led here, not
			// whatever a race might have changed it to in between.
			func(others []RegisteredWorktree, preferred string) (string, error) {
				return AllocateSlug(*identity, r.domain, others, preferred)
			},
			writer,
			func() (WorktreeInitialization, error) {
				exists, err := files.PathExists(WorktreeHome(r.ControlHome, identity.RepositoryID, identity.WorktreeID))
				if err != nil {
					return "", err
				}
				if exists {
					return Complete, nil
				}
				return Pending, nil
			},
			labelNamespace,
		)
		if err != nil {
			return err
		}

		result := EnsuredRegistration{WorktreeRegistration: registration(*currentIdentity, record.RegisteredWorktree)}
		if record.PreviousLabelNamespace != nil {
			result.PreviousLabelNamespace = record.PreviousLabelNamespace
			namespace := ""
			if record.LabelNamespace != nil {
				namespace = *record.LabelNamespace
			}
			// `up` only lets this happen once nothing is left under the old
			// namespace, so nothing is orphaned.
			r.logger.Info(fmt.Sprintf(
				"Now labelling this worktree's resources under %s (was %s; nothing remained under it).",
				namespace, *record.PreviousLabelNamespace,
			))
		}
		if record.PreviousHost != nil {
			result.PreviousHost = record.PreviousHost
			r.logger.Warn(fmt.Sprintf(
				"This worktree moved from %s to %s. Run '%s up' to route it at its new hostname.",
				*record.PreviousHost, layout.WorktreeHost(record.Slug, record.Domain), r.commandName,
			))
		}
		ensured = &result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ensured, nil
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
	// No domain means no treerig.toml was loaded (a command that runs without
	// one), so there is nothing for the record to disagree with. The invariant
	// that makes "" a safe sentinel: a loaded config always has a domain.
	if r.domain != "" && record.Domain != r.domain {
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
