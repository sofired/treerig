package checkoutindex

// The service that ties this package's pure snapshot and render logic to
// disk, Docker, and the router's own lock (#41). It regenerates the page
// after every lifecycle verb, best-effort: a stale index is an
// inconvenience, a failed `up` because of it would not be.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktreestate"
)

// defaultLockTimeout bounds how long a refresh waits for the router lock:
// the index is best-effort, so a daemon that will not answer must not hold
// the router
// lock for lock.RouterTimeout's full 20 minutes — that budget is sized for
// router.Ensure's own worst case (a first start, a recreate, every probe
// around them), not for a refresh that only ever runs after something else
// has already finished. 60s is long enough not to give up on a daemon that
// is merely busy (`up` gives the same one 45s to bring a stack up) while
// keeping RefreshBestEffort's callers — down, worktree remove, `up` itself —
// from stalling behind it. Overridable per Service (Options.LockTimeout) for
// a test that wants a shorter one.
const defaultLockTimeout = 60 * time.Second

// defaultCardConcurrency bounds how many index cards Snapshot reads at once
// (#18): generous for an ordinary machine's handful of worktrees, small
// enough that even a control home with thousands of registrations stays
// well under a low ulimit -n. Overridable per Service for a test that wants
// to observe the bound itself.
const defaultCardConcurrency = 32

// indexHost is where the checkout index answers, matching
// internal/router/stack.go's own IndexHost; duplicated rather than imported,
// since internal/router already imports this package for PlaceholderHTML,
// and the reverse import would cycle.
const indexHost = brand.Name + ".localhost"

// Registry is what the checkout index reads from the machine-wide worktree
// registry — the slice of *registry.Registry this package actually calls,
// declared here because this package is the consumer; a test supplies its
// own double.
type Registry interface {
	ListAll(ctx context.Context) ([]registry.RegisteredWorktree, error)
}

// Identities is what the checkout index reads about the current repository's
// worktrees, answering for whichever checkout the service was started from —
// registry.Registry's own Identities field satisfies this.
type Identities interface {
	Entries(ctx context.Context) ([]registry.WorktreeEntry, error)
	Repository(ctx context.Context, worktree string) (id string, present, inside bool, err error)
}

// Options configure a Service.
type Options struct {
	ControlHome string
	// RouterPort is the port the router publishes; a card's service URL on
	// any other port is not this checkout's (serviceBelongsToHost).
	RouterPort int
	// IndexFile is where the rendered page is written: the shared router's
	// own layout.RouterState(...).IndexFile, never anything under a control
	// home — the router's state is machine-wide, per treerig root, not per
	// control home (see internal/layout's RouterStateDir comment).
	// Resolving that path needs an Env's Getenv, which this package does
	// not depend on, so the caller resolves it once and hands it over.
	// Its directory, the one the index container serves, also receives the
	// favicon files (WriteIcons) the page links.
	IndexFile string
	// Lock is the shared router's lock scope (internal/commands'
	// routerLockScope): Refresh takes it around reading the registry and
	// Docker and writing the page,
	// the same lock router.Ensure holds around bringing the router itself
	// up, so the two can never observe or write the page in the middle of
	// each other's work.
	Lock       lock.Scope
	Registry   Registry
	Identities Identities
	// IdentityRoot is the checkout Identities answers for; named in every
	// message about it being unable to answer, the way
	// registry.Registry.Identities.Root is here.
	IdentityRoot string
	Runner       run.Runner
	Logger       logger.Logger
	// Now defaults to time.Now; a test pins it.
	Now func() time.Time
	// Invocation names this process to whoever waits on the lock.
	Invocation string
	// CardConcurrency bounds concurrent card reads (#18); zero uses defaultCardConcurrency.
	CardConcurrency int
	// LockTimeout bounds how long Refresh waits for the router lock; zero
	// uses defaultLockTimeout, never lock.RouterTimeout — see
	// defaultLockTimeout's own comment.
	LockTimeout time.Duration
	// InventoryTimeout bounds one `docker ps` call; zero uses
	// worktreestate.DefaultInventoryTimeout. A test overrides it to observe the
	// bound itself without actually waiting on it.
	InventoryTimeout time.Duration
	// Mounts reads the mount table the worktrees' directories are judged
	// against; nil reads the machine's. A test supplies its own.
	Mounts func() (worktreestate.Mounts, error)
}

// Service is the checkout index tied to disk, Docker, and the shared
// router's lock. It satisfies commands.Index.
type Service struct {
	opts Options
	// readCard is card.ReadIndexCard, indirected only so a test can inject a
	// fake that counts concurrent in-flight calls or fails one on purpose,
	// without needing real file descriptor exhaustion to exercise #18. Never
	// set outside a test.
	readCard func(controlHome, repositoryID, worktreeID string) (*card.IndexCard, error)
	// writeIcons is WriteIcons, indirected only so a test can fail one write
	// where the filesystem would not, or watch the order of writes. Never set
	// outside a test.
	writeIcons func(dir string) (warning, err error)
}

// NewService builds a Service from opts, filling in defaults.
func NewService(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CardConcurrency <= 0 {
		opts.CardConcurrency = defaultCardConcurrency
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = defaultLockTimeout
	}
	if opts.Logger == nil {
		opts.Logger = logger.Silent{}
	}
	return &Service{opts: opts, readCard: card.ReadIndexCard, writeIcons: WriteIcons}
}

// URL is where the page this Service maintains can be read.
func (s *Service) URL() string {
	if s.opts.RouterPort == 80 {
		return "http://" + indexHost
	}
	return fmt.Sprintf("http://%s:%d", indexHost, s.opts.RouterPort)
}

// cardRead is one registration's card, or whether reading it failed.
type cardRead struct {
	record registry.RegisteredWorktree
	card   *card.IndexCard
	failed bool
	// err is failed's own reason; nil unless failed. Kept off the result
	// consumers outside this file read (they only ever branch on failed),
	// and set only on the copy readCards collects into failures, for
	// warnCardReadFailures to report.
	err error
}

// readCards reads every registration's index card, bounded to at most
// CardConcurrency reads in flight at once (#18): thousands of registrations
// means thousands of open files, and reading them all at once is exactly
// what exhausts a low ulimit -n. A semaphore over a plain WaitGroup, rather
// than errgroup.SetLimit (which containers() now uses for its own, unbounded
// fan-out), is deliberate here: a card read never fails this method — a
// failure is recorded per registration, not returned — so there is no error
// to propagate and nothing for errgroup's own cancellation to buy this loop.
//
// A card that fails to read (as opposed to one that was never written —
// ReadIndexCard's own (nil, nil) case) is warned about once here and left
// for BuildSnapshot to render as Unknown, never as if the worktree had
// simply never come up.
func (s *Service) readCards(registrations []registry.RegisteredWorktree) []cardRead {
	results := make([]cardRead, len(registrations))
	semaphore := make(chan struct{}, s.opts.CardConcurrency)
	var wg sync.WaitGroup
	wg.Add(len(registrations))
	// A warning is collected, not issued, inside each goroutine (#18's own
	// scenario — thousands of registrations, an EMFILE-style exhaustion — is
	// exactly the case that would otherwise flood stderr with one line per
	// failed read): warnCardReadFailures below emits exactly one, after
	// every goroutine has finished, scanning results in registration order
	// so "the first failure" it names is deterministic — not whichever
	// goroutine happened to finish first.
	for i, record := range registrations {
		semaphore <- struct{}{}
		go func(i int, record registry.RegisteredWorktree) {
			defer wg.Done()
			defer func() { <-semaphore }()
			read, err := s.readCard(s.opts.ControlHome, record.RepositoryID, record.WorktreeID)
			results[i] = cardRead{record: record, card: read, failed: err != nil, err: err}
		}(i, record)
	}
	wg.Wait()
	s.warnCardReadFailures(results)
	return results
}

// warnCardReadFailures is readCards' one warning for however many of results
// failed to read: singular or plural to match cardReadFailureNotice's own
// house style, naming the count and the first failure (in registration
// order) with its own checkout and error — enough to start investigating
// without one line per failed read.
func (s *Service) warnCardReadFailures(results []cardRead) {
	count := 0
	var first cardRead
	for _, result := range results {
		if !result.failed {
			continue
		}
		if count == 0 {
			first = result
		}
		count++
	}
	switch count {
	case 0:
		return
	case 1:
		s.opts.Logger.Warn(fmt.Sprintf(
			"Could not read one checkout's index card: %s.%s: %s",
			first.record.Slug, first.record.Domain, first.err,
		))
	default:
		s.opts.Logger.Warn(fmt.Sprintf(
			"Could not read %d checkouts' index cards; the first was %s.%s: %s",
			count, first.record.Slug, first.record.Domain, first.err,
		))
	}
}

// namespaces is every label namespace Snapshot must ask Docker about.
func (s *Service) namespaces(reads []cardRead) []string {
	registrations := make([]registry.RegisteredWorktree, len(reads))
	cards := make([]*card.IndexCard, len(reads))
	for i, read := range reads {
		registrations[i], cards[i] = read.record, read.card
	}
	return worktreestate.Namespaces(s.opts.ControlHome, registrations, cards)
}

// Snapshot reads the registry, every worktree's card, Git's live worktrees,
// and Docker's container inventory, classifies every registration with
// worktreestate, and builds one IndexSnapshot from them. It takes no lock of
// its own; Refresh is what a caller actually wants.
func (s *Service) Snapshot(ctx context.Context) (IndexSnapshot, error) {
	registrations, err := s.opts.Registry.ListAll(ctx)
	if err != nil {
		return IndexSnapshot{}, err
	}

	reads := s.readCards(registrations)
	cards := make(map[string]card.IndexCard, len(reads))
	cardReadFailures := make(map[string]struct{})
	for _, read := range reads {
		key := EntryKey(read.record.RepositoryID, read.record.WorktreeID)
		if read.card != nil {
			cards[key] = *read.card
		}
		if read.failed {
			cardReadFailures[key] = struct{}{}
		}
	}

	var identities []registry.WorktreeIdentity
	var currentRepositoryID *string
	var identityError *IdentityFailureInput
	var repositories map[string]worktreestate.Repository
	// Outside every Git work tree there is no current repository to be
	// unable to identify: the page then lists every worktree, as it does for
	// a repository treerig has no identity for.
	repositoryID, present, _, err := s.opts.Identities.Repository(ctx, s.opts.IdentityRoot)
	switch {
	case err != nil:
		reason := fmt.Sprintf("Git could not identify the repository at %s: %s", s.opts.IdentityRoot, err)
		identityError = &IdentityFailureInput{Reason: reason}
		s.opts.Logger.Warn(reason + ". The overview cannot tell which of its worktrees have lost their directories.")
	case present:
		currentRepositoryID = &repositoryID
		// Asked once, here, so the identities the rows read their branch and
		// root from and the evidence their state is told from are one answer.
		entries, entriesErr := s.opts.Identities.Entries(ctx)
		if entriesErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return IndexSnapshot{}, ctxErr
			}
			reason := fmt.Sprintf("Git could not list the worktrees of %s: %s", s.opts.IdentityRoot, entriesErr)
			identityError = &IdentityFailureInput{Reason: reason}
			s.opts.Logger.Warn(reason + ". The overview shows its worktrees as unknown.")
		}
		identities = identitiesOf(repositoryID, entries)
		repositories = map[string]worktreestate.Repository{repositoryID: worktreestate.Answered{Listed: entries, Err: entriesErr}}
	}

	subjects := make([]worktreestate.Subject, len(reads))
	for i, read := range reads {
		subjects[i] = s.subjectOf(read)
	}
	report, err := worktreestate.New(worktreestate.Options{
		ControlHome: s.opts.ControlHome, Runner: s.opts.Runner,
		InventoryTimeout: s.opts.InventoryTimeout, Mounts: s.opts.Mounts,
	}).Gather(ctx, worktreestate.Request{Subjects: subjects, Namespaces: s.namespaces(reads), Repositories: repositories})
	if err != nil {
		return IndexSnapshot{}, err
	}
	var dockerError *string
	if !report.Docker.Answered {
		reason := report.Docker.Error
		dockerError = &reason
		s.opts.Logger.Warn("The overview cannot show what runs because containers could not be listed: " + reason)
	}

	sharedRecords, sharedProblems, err := registry.ScanSharedRecords(s.opts.ControlHome)
	if err != nil {
		// The shared group is an addition to the page; a control home whose
		// shared records cannot be listed still gets its worktrees.
		sharedRecords = nil
		s.opts.Logger.Warn(fmt.Sprintf("The index leaves out shared services because their records could not be listed: %s", err))
	}
	for _, problem := range sharedProblems {
		s.opts.Logger.Warn(fmt.Sprintf("The index leaves out a shared service whose record could not be read: %s: %s", problem.File, problem.Reason))
	}

	return BuildSnapshot(SnapshotInput{
		Registrations:       registrations,
		SharedRecords:       sharedRecords,
		Cards:               cards,
		Identities:          identities,
		CurrentRepositoryID: currentRepositoryID,
		Containers:          report.Docker.Containers,
		States:              report.States,
		DockerError:         dockerError,
		RouterPort:          s.opts.RouterPort,
		IdentityError:       identityError,
		CardReadFailures:    cardReadFailures,
		GeneratedAt:         s.opts.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}), nil
}

// identitiesOf is the live worktrees among entries: those whose directory is
// there and which carry an identity of this repository.
func identitiesOf(repositoryID string, entries []registry.WorktreeEntry) []registry.WorktreeIdentity {
	var identities []registry.WorktreeIdentity
	for _, entry := range entries {
		if entry.Exists && entry.WorktreeID != "" && entry.RepositoryID == repositoryID {
			identities = append(identities, registry.WorktreeIdentity{
				Root: entry.Root, Branch: entry.Branch, RepositoryID: entry.RepositoryID, WorktreeID: entry.WorktreeID,
			})
		}
	}
	return identities
}

// subjectOf is what the Gatherer is told about one registration read by
// readCards.
func (s *Service) subjectOf(read cardRead) worktreestate.Subject {
	return worktreestate.SubjectOf(read.record, read.card, read.err)
}

// Refresh rebuilds the page under the shared router's lock and writes it to
// IndexFile, returning the snapshot it wrote. The lock matches the one
// router.Ensure holds around bringing the router up (internal/commands'
// routerLockScope), taken here the same way — lock.New, Acquire, Release —
// rather than through internal/lifecycle's full signal-deferring Hold:
// Refresh runs after a mutation's own locks have been released (or, from
// `router up`, after the router's own hold has) — with one sanctioned
// exception: a `dev` session refreshes while it still holds its worktree's
// development lock (docs/design/locks.md), which is safe because no router
// lock is ever held while waiting for a worktree-level lock. So it needs the
// mutual exclusion a plain Lock gives it, not another layer of the same
// signal handling built for a whole command invocation. It waits only
// Options.LockTimeout (defaultLockTimeout, 60 s) for that lock, not
// lock.RouterTimeout: a best-effort refresh gives up rather than stall the
// command that asked for it behind a router start.
func (s *Service) Refresh(ctx context.Context) (IndexSnapshot, error) {
	lk := lock.New(s.opts.Lock, lock.Options{Timeout: s.opts.LockTimeout, Holder: s.opts.Invocation, Logger: s.opts.Logger})
	lockCtx, err := lk.Acquire(ctx, s.opts.Invocation)
	if err != nil {
		return IndexSnapshot{}, err
	}
	defer func() {
		if lk.Lost() {
			_ = lk.Abandon()
			return
		}
		_ = lk.Release()
	}()

	snapshot, err := s.Snapshot(lockCtx)
	if err != nil {
		return IndexSnapshot{}, err
	}
	// The icons first: the page links them, so it must never be served while
	// one is missing. A failed write over an icon an earlier refresh left is
	// only a warning (the page still refreshes, with the older icon); with an
	// icon missing the page is left alone and the error returned.
	if err := ensureIcons(filepath.Dir(s.opts.IndexFile), s.opts.Logger, s.writeIcons); err != nil {
		return IndexSnapshot{}, err
	}
	if err := files.AtomicWrite(s.opts.IndexFile, []byte(RenderIndexHTML(snapshot)), 0o644); err != nil {
		return IndexSnapshot{}, err
	}
	return snapshot, nil
}

// RefreshBestEffort satisfies commands.Index: it regenerates the page,
// warning rather than failing the caller if it cannot. reason names what
// just happened ("up", "down", "worktree remove", ...), for the warning
// alone; nothing is printed when the refresh succeeds.
func (s *Service) RefreshBestEffort(ctx context.Context, reason string) {
	if _, err := s.Refresh(ctx); err != nil {
		s.opts.Logger.Warn(fmt.Sprintf("Could not refresh the checkout index after %s: %s", reason, err))
	}
}
