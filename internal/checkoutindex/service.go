package checkoutindex

// The service that ties this package's pure snapshot and render logic to
// disk, Docker, and the router's own lock: what src/checkout-index/
// service.ts's CheckoutIndexService is on the TypeScript side (#41). It
// regenerates the page after every lifecycle verb, best-effort: a stale
// index is an inconvenience, a failed `up` because of it would not be.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
)

// defaultInventoryTimeout bounds one `docker ps` call: 30s, matching
// doctor.ts's own DOCKER_PROBE_TIMEOUT_MS for the same kind of introspection
// call, and the same 30s every other docker call this port makes for an
// introspection call (internal/backend/compose.go's own timeouts comment).
// Overridable per Service (Options.InventoryTimeout) for a test that
// wants to observe the bound itself without actually waiting on it.
const defaultInventoryTimeout = 30 * time.Second

// defaultLockTimeout is service.ts's own INDEX_LOCK_TIMEOUT_MS: the index is
// best-effort, so a daemon that will not answer must not hold the router
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
// live worktrees, answering for whichever checkout the service was started
// from — registry.Registry's own Identities field satisfies this.
type Identities interface {
	ActiveIdentities(ctx context.Context) ([]registry.WorktreeIdentity, error)
	RepositoryID(ctx context.Context, worktree string) (id string, present bool, err error)
}

// Options configure a Service.
type Options struct {
	ControlHome string
	// RouterPort is the port the router publishes; a card's service URL on
	// any other port is not this checkout's (serviceBelongsToHost).
	RouterPort int
	// IndexFile is where the rendered page is written: the shared router's
	// own layout.RouterState(...).IndexFile, never anything under a control
	// home — the router's state is machine-wide, per berth root, not per
	// control home (see internal/layout's RouterStateDir comment).
	// Resolving that path needs an Env's Getenv, which this package does
	// not depend on, so the caller resolves it once and hands it over.
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
	// uses defaultLockTimeout (service.ts's INDEX_LOCK_TIMEOUT_MS), never
	// lock.RouterTimeout — see defaultLockTimeout's own comment.
	LockTimeout time.Duration
	// InventoryTimeout bounds one `docker ps` call; zero uses defaultInventoryTimeout.
	InventoryTimeout time.Duration
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
	if opts.InventoryTimeout <= 0 {
		opts.InventoryTimeout = defaultInventoryTimeout
	}
	if opts.Logger == nil {
		opts.Logger = logger.Silent{}
	}
	return &Service{opts: opts, readCard: card.ReadIndexCard}
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

// namespaces is every label namespace Snapshot must ask Docker about: this
// build's own, plus one per registration, recorded on the registration
// itself, on the card just read (which may be more current, if the
// registration predates namespace tracking), or found on disk wherever the
// worktree's home currently is.
func (s *Service) namespaces(reads []cardRead) []string {
	seen := map[string]struct{}{}
	var namespaces []string
	add := func(namespace string) {
		if namespace == "" {
			return
		}
		if _, ok := seen[namespace]; !ok {
			seen[namespace] = struct{}{}
			namespaces = append(namespaces, namespace)
		}
	}
	add(brand.LabelNamespace)
	for _, read := range reads {
		switch {
		case read.record.LabelNamespace != nil:
			add(*read.record.LabelNamespace)
		case read.card != nil:
			add(read.card.LabelNamespace)
		default:
			if namespace := card.RecordedLabelNamespace(s.opts.ControlHome, card.Registration{
				RepositoryID: read.record.RepositoryID, WorktreeID: read.record.WorktreeID, LabelNamespace: read.record.LabelNamespace,
			}); namespace != nil {
				add(*namespace)
			}
		}
	}
	return namespaces
}

// inventory runs `docker ps` for one label namespace: the containers it
// named, the reason it could not answer (Docker unavailable, a non-zero
// exit, a timeout, or an inventory line it could not parse), or a genuine
// error that must abort the whole refresh rather than read as "Docker could
// not be asked" — a cancellation, most pointedly, which service.ts's own
// inventory() lets through unchanged rather than folding into its string
// return.
func (s *Service) inventory(ctx context.Context, namespace string) (records []ContainerRecord, reason string, err error) {
	format := strings.Join([]string{
		"{{.State}}", "{{.Status}}",
		fmt.Sprintf(`{{.Label "%s.repository-uuid"}}`, namespace),
		fmt.Sprintf(`{{.Label "%s.worktree-uuid"}}`, namespace),
		`{{.Label "com.docker.compose.service"}}`,
	}, "\t")
	result, err := docker.Run(ctx, s.opts.Runner, []string{
		"ps", "--all", "--filter", "label=" + namespace + ".managed=true", "--format", format,
	}, run.Spec{AllowFailure: true, Timeout: s.opts.InventoryTimeout})
	if err != nil {
		var unavailable *docker.UnavailableError
		if errors.As(err, &unavailable) {
			return nil, unavailable.Error(), nil
		}
		return nil, "", err
	}
	if result.TimedOut {
		return nil, fmt.Sprintf("`docker ps` did not answer within %ds", int(s.opts.InventoryTimeout.Seconds())), nil
	}
	if result.ExitCode != 0 {
		return nil, docker.Said(result.Stderr, fmt.Sprintf("`docker ps` exited %d without saying why", result.ExitCode)), nil
	}
	records, parseErr := ParseContainerInventory(result.Stdout)
	if parseErr != nil {
		return nil, parseErr.Error(), nil
	}
	return records, "", nil
}

// containerAnswer is one namespace's inventory() answer, kept at its
// original index so containers can merge them back in namespace order
// however the goroutines that fetched them finished.
type containerAnswer struct {
	records []ContainerRecord
	reason  string
}

// containers asks Docker about every namespace concurrently — service.ts's
// own containers() does the same with Promise.all — returning the merged
// inventory or the first failure reported, in namespace order, however many
// namespaces there were. errgroup.WithContext cancels every other namespace's
// still-running `docker ps` the moment one inventory() call returns a real
// error (Docker unavailable is not one: inventory() already turns that into
// a reason string, not an error), so a genuine failure — most pointedly the
// caller's own context being cancelled — aborts the whole refresh promptly
// rather than waiting out every remaining namespace's own timeout, and
// Wait() reports whichever error actually happened first.
func (s *Service) containers(ctx context.Context, namespaces []string) ([]ContainerRecord, *string, error) {
	answers := make([]containerAnswer, len(namespaces))
	group, groupCtx := errgroup.WithContext(ctx)
	for i, namespace := range namespaces {
		group.Go(func() error {
			records, reason, err := s.inventory(groupCtx, namespace)
			if err != nil {
				return err
			}
			answers[i] = containerAnswer{records: records, reason: reason}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	for _, answer := range answers {
		if answer.reason != "" {
			s.opts.Logger.Warn("The index shows service state as Unknown because containers could not be listed: " + answer.reason)
			reason := answer.reason
			return nil, &reason, nil
		}
	}
	// Non-nil from the start: SnapshotInput.Containers uses nil specifically
	// to mean Docker could not be asked at all (see ParseContainerInventory's
	// own comment), and appending zero records from an empty namespace's
	// answer must not silently turn a successful, empty inventory back into
	// that same nil.
	all := []ContainerRecord{}
	for _, answer := range answers {
		all = append(all, answer.records...)
	}
	return all, nil, nil
}

// Snapshot reads the registry, every worktree's card, Git's live identities,
// and Docker's container inventory, and builds one IndexSnapshot from them.
// It takes no lock of its own; Refresh is what a caller actually wants.
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
	repositoryID, present, err := s.opts.Identities.RepositoryID(ctx, s.opts.IdentityRoot)
	switch {
	case err != nil:
		reason := fmt.Sprintf("Git could not identify the repository at %s: %s", s.opts.IdentityRoot, err)
		identityError = &IdentityFailureInput{Reason: reason}
		s.opts.Logger.Warn(reason + ". The index cannot tell which of its checkouts are missing.")
	case present:
		currentRepositoryID = &repositoryID
		identities, err = s.opts.Identities.ActiveIdentities(ctx)
		if err != nil {
			identities = nil
			reason := fmt.Sprintf("Git could not list the worktrees of %s: %s", s.opts.IdentityRoot, err)
			identityError = &IdentityFailureInput{Reason: reason}
			s.opts.Logger.Warn(reason + ". The index shows its checkouts as unknown.")
		}
	}

	namespaces := s.namespaces(reads)
	containerRecords, dockerError, err := s.containers(ctx, namespaces)
	if err != nil {
		return IndexSnapshot{}, err
	}

	return BuildSnapshot(SnapshotInput{
		Registrations:       registrations,
		Cards:               cards,
		Identities:          identities,
		CurrentRepositoryID: currentRepositoryID,
		Containers:          containerRecords,
		DockerError:         dockerError,
		RouterPort:          s.opts.RouterPort,
		IdentityError:       identityError,
		CardReadFailures:    cardReadFailures,
		GeneratedAt:         s.opts.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}), nil
}

// Refresh rebuilds the page under the shared router's lock and writes it to
// IndexFile, returning the snapshot it wrote. The lock matches the one
// router.Ensure holds around bringing the router up (internal/commands'
// routerLockScope), taken here the same way — lock.New, Acquire, Release —
// rather than through internal/lifecycle's full signal-deferring Hold:
// Refresh always runs after a mutation's own lock has already been released
// (or, from `router up`, after the router's own hold has), so it needs the
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
