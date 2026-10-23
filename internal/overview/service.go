package overview

// The service that ties this package's pure snapshot and render logic to
// disk, Docker, and the router's own lock (#41). It regenerates the page
// after every lifecycle verb, best-effort: a stale index is an
// inconvenience, a failed `up` because of it would not be.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
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

// indexHost is where the overview answers, matching
// internal/router/stack.go's own IndexHost; duplicated rather than imported,
// since internal/router already imports this package for PlaceholderHTML,
// and the reverse import would cycle.
const indexHost = brand.Name + ".localhost"

// Registry is what the overview reads from the machine-wide worktree
// registry — the slice of *registry.Registry this package actually calls,
// declared here because this package is the consumer; a test supplies its
// own double.
type Registry interface {
	ListAll(ctx context.Context) ([]registry.RegisteredWorktree, error)
}

// Identities is what the overview reads about the current repository's
// worktrees, answering for whichever directory the service was started from —
// registry.Registry's own Identities field satisfies this.
type Identities interface {
	Entries(ctx context.Context) ([]registry.WorktreeEntry, error)
	Repository(ctx context.Context, worktree string) (id string, present, inside bool, err error)
}

// Options configure a Service.
type Options struct {
	ControlHome string
	// RouterPort is the port the router publishes; a card's service URL on
	// any other port is not this worktree's (serviceBelongsToHost).
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
	// IdentityRoot is the worktree Identities answers for; named in every
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
	// QuietDocker leaves out Refresh's warning that Docker could not be asked,
	// for a caller that says why itself (`list` ends in the cause), so the
	// cause is given once.
	QuietDocker bool
	// WarnOfRepositories says in a warning which repositories Git could not
	// be asked about. A refresh after `up` leaves it out: a repository that is
	// offline for good would otherwise be reported after every command.
	WarnOfRepositories bool
	// GitTimeout bounds everything asked of Git in one snapshot, all
	// repositories together; zero uses worktreestate.DefaultGitTimeout. A
	// repository that has not answered by then reads unknown.
	GitTimeout time.Duration
	// Environ is the process environment Git is run with
	// (registry.NewIdentityServiceEnviron); nil inherits the process's.
	Environ []string
}

// Service is the overview tied to disk, Docker, and the shared
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
// order) with its own worktree and error — enough to start investigating
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
			"Could not read one worktree's index card: %s.%s: %s",
			first.record.Slug, first.record.Domain, first.err,
		))
	default:
		s.opts.Logger.Warn(fmt.Sprintf(
			"Could not read %d worktrees' index cards; the first was %s.%s: %s",
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
// its own, and writes nothing; Refresh is what a caller who wants the page
// written actually wants.
//
// Docker and Git are asked at the same time, each within its bound. Git is
// asked about every repository that has a registration: the one the service
// runs in through its Identities, any other through the directories its cards
// recorded (worktreestate.AskRepositories). A repository that has not answered
// within Options.GitTimeout reads unknown, and so does a Docker that has not.
func (s *Service) Snapshot(ctx context.Context) (IndexSnapshot, error) {
	return s.SnapshotOf(ctx, "")
}

// EmptySnapshot is the snapshot of nothing: no worktree, no shared service, and
// Docker not asked, since nothing needed it.
func (s *Service) EmptySnapshot() IndexSnapshot {
	return IndexSnapshot{GeneratedAt: s.generatedAt(), DockerReachable: true}
}

// generatedAt is the service's clock as the page spells it.
func (s *Service) generatedAt() string {
	return s.opts.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// SnapshotOf is Snapshot for the registrations of one repository, by its ID;
// an empty ID is every repository on the machine. Only the repository's own
// Git is asked, and only its shared services are listed.
func (s *Service) SnapshotOf(ctx context.Context, onlyRepositoryID string) (IndexSnapshot, error) {
	registrations, err := s.opts.Registry.ListAll(ctx)
	if err != nil {
		return IndexSnapshot{}, err
	}
	if onlyRepositoryID != "" {
		registrations = slices.DeleteFunc(registrations, func(record registry.RegisteredWorktree) bool {
			return record.RepositoryID != onlyRepositoryID
		})
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

	sharedRecords, sharedProblems, err := registry.ScanSharedRecords(s.opts.ControlHome)
	if err != nil {
		// The shared group is an addition to the page; a control home whose
		// shared records cannot be listed still gets its worktrees.
		sharedRecords = nil
		s.opts.Logger.Warn(fmt.Sprintf("The overview leaves out shared services because their records could not be listed: %s", err))
	}
	for _, problem := range sharedProblems {
		s.opts.Logger.Warn(fmt.Sprintf("The overview leaves out a shared service whose record could not be read: %s: %s", problem.File, problem.Reason))
	}
	if onlyRepositoryID != "" {
		sharedRecords = slices.DeleteFunc(sharedRecords, func(record registry.SharedRecord) bool {
			return record.RepositoryID != onlyRepositoryID
		})
	}

	// Nothing to classify and nothing to show a state of: Docker and Git have
	// nothing to be asked about.
	if len(reads) == 0 && len(sharedRecords) == 0 {
		empty := s.EmptySnapshot()
		empty.Entries = []IndexEntry{}
		return empty, nil
	}

	gatherer := worktreestate.New(worktreestate.Options{
		ControlHome: s.opts.ControlHome, Runner: s.opts.Runner,
		InventoryTimeout: s.opts.InventoryTimeout, Mounts: s.opts.Mounts,
		GitTimeout: s.opts.GitTimeout, Environ: s.opts.Environ,
	})
	var inventory worktreestate.Inventory
	var inventoryErr error
	var git gitEvidence
	var gitErr error
	var phase sync.WaitGroup
	phase.Go(func() { inventory, inventoryErr = gatherer.Inventory(ctx, s.namespaces(reads)) })
	phase.Go(func() { git, gitErr = s.askGit(ctx, gatherer, reads, onlyRepositoryID) })
	phase.Wait()
	if inventoryErr != nil {
		return IndexSnapshot{}, inventoryErr
	}
	if gitErr != nil {
		return IndexSnapshot{}, gitErr
	}

	subjects := make([]worktreestate.Subject, len(reads))
	for i, read := range reads {
		subjects[i] = s.subjectOf(read)
	}
	report, err := gatherer.Gather(ctx, worktreestate.Request{
		Subjects: subjects, Repositories: git.repositories, Inventory: &inventory,
	})
	if err != nil {
		return IndexSnapshot{}, err
	}
	var dockerError *string
	if !report.Docker.Answered {
		reason := report.Docker.Error
		dockerError = &reason
	}

	return BuildSnapshot(SnapshotInput{
		Registrations:       registrations,
		SharedRecords:       sharedRecords,
		Cards:               cards,
		Identities:          git.identities,
		CurrentRepositoryID: git.current,
		Containers:          report.Docker.Containers,
		States:              report.States,
		DockerError:         dockerError,
		RouterPort:          s.opts.RouterPort,
		IdentityError:       git.identityError,
		CardReadFailures:    cardReadFailures,
		GitHints:            git.hints,
		GeneratedAt:         s.generatedAt(),
	}), nil
}

// gitEvidence is everything a snapshot asked of Git: which repository the
// service runs in, the worktrees Git lists, and what to say of the ones it
// could not be asked about.
type gitEvidence struct {
	current       *string
	identityError *IdentityFailureInput
	identities    []registry.WorktreeIdentity
	repositories  map[string]worktreestate.Repository
	hints         []string
}

// askGit asks Git about the repository the service runs in, and, unless the
// snapshot is of that one alone (onlyRepositoryID), every other repository
// with a registration, all within one bound. Only a cancellation is an error.
func (s *Service) askGit(ctx context.Context, gatherer *worktreestate.Gatherer, reads []cardRead, onlyRepositoryID string) (gitEvidence, error) {
	evidence := gitEvidence{repositories: map[string]worktreestate.Repository{}}
	budget := s.opts.GitTimeout
	if budget <= 0 {
		budget = worktreestate.DefaultGitTimeout
	}

	var others []worktreestate.RepositoryAnswer
	var othersErr error
	var group sync.WaitGroup
	// The current repository is asked while the others are, so the phase costs
	// one bound and not two. Its answer is not known yet, so the others are
	// every repository with a registration; the current one's ID is taken out
	// of them below, when it is.
	asks := s.repositoryAsks(reads)
	if onlyRepositoryID == "" {
		group.Go(func() { others, othersErr = gatherer.AskRepositories(ctx, asks) })
	}

	type current struct {
		id      string
		present bool
		entries []registry.WorktreeEntry
		listErr error
		err     error
	}
	// Repository and Entries are one question to the bound: a mount that hangs
	// under either hangs the command the same.
	found, err := worktreestate.Within(ctx, budget, func(ctx context.Context) (current, error) {
		id, present, _, err := s.opts.Identities.Repository(ctx, s.opts.IdentityRoot)
		if err != nil || !present {
			return current{id: id, present: present, err: err}, nil
		}
		entries, listErr := s.opts.Identities.Entries(ctx)
		return current{id: id, present: present, entries: entries, listErr: listErr}, nil
	})
	if err == nil {
		err = found.err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		group.Wait()
		return gitEvidence{}, ctxErr
	}
	switch {
	case err != nil:
		reason := fmt.Sprintf("Git could not identify the repository at %s: %s", s.opts.IdentityRoot, err)
		evidence.identityError = &IdentityFailureInput{Reason: reason}
		s.opts.Logger.Warn(reason + ". Which worktrees have lost their directories can't be told.")
	case found.present:
		id := found.id
		evidence.current = &id
		if found.listErr != nil {
			reason := fmt.Sprintf("Git could not list the worktrees of %s: %s", s.opts.IdentityRoot, found.listErr)
			evidence.identityError = &IdentityFailureInput{Reason: reason}
			s.opts.Logger.Warn(reason + ". Their states show unknown.")
			evidence.hints = append(evidence.hints, worktreestate.GitHint(found.listErr))
		}
		evidence.identities = identitiesOf(id, found.entries)
		evidence.repositories[id] = worktreestate.Answered{Listed: found.entries, Err: found.listErr}
	}
	group.Wait()
	if othersErr != nil {
		return gitEvidence{}, othersErr
	}

	failed := 0
	var first error
	for _, answer := range others {
		if !answer.Asked || (evidence.current != nil && answer.ID == *evidence.current) {
			continue
		}
		evidence.repositories[answer.ID] = worktreestate.Answered{Listed: answer.Entries, Err: answer.Err}
		evidence.identities = append(evidence.identities, identitiesOf(answer.ID, answer.Entries)...)
		if answer.Err != nil {
			if failed == 0 {
				first = answer.Err
			}
			failed++
			evidence.hints = append(evidence.hints, worktreestate.GitHint(answer.Err))
		}
	}
	if failed > 0 && s.opts.WarnOfRepositories {
		s.opts.Logger.Warn(fmt.Sprintf("Git could not list the worktrees of %d %s, so their states show unknown; the first said: %s",
			failed, plural(failed, "repository", "repositories"), text.OneLine(first.Error(), 300)))
	}
	return evidence, nil
}

// repositoryAsks is one ask per repository with a registration, in the order of
// their IDs, with the directories its cards recorded, in the order read.
func (s *Service) repositoryAsks(reads []cardRead) []worktreestate.RepositoryAsk {
	byID := map[string]*worktreestate.RepositoryAsk{}
	for _, read := range reads {
		id := read.record.RepositoryID
		ask, seen := byID[id]
		if !seen {
			ask = &worktreestate.RepositoryAsk{ID: id}
			byID[id] = ask
		}
		if read.card != nil && read.card.Root != "" && !slices.Contains(ask.Roots, read.card.Root) {
			ask.Roots = append(ask.Roots, read.card.Root)
		}
	}
	asks := make([]worktreestate.RepositoryAsk, 0, len(byID))
	for _, ask := range byID {
		asks = append(asks, *ask)
	}
	slices.SortFunc(asks, func(left, right worktreestate.RepositoryAsk) int { return strings.Compare(left.ID, right.ID) })
	return asks
}

// plural is singular for one and many otherwise.
func plural(count int, singular, many string) string {
	if count == 1 {
		return singular
	}
	return many
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

// Refresh takes a snapshot, with no lock held, and writes it to IndexFile under
// the shared router's lock, returning the snapshot it wrote. The lock matches the one
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
	// Read first, and under no lock: Git and Docker can take as long as their
	// bounds, and the router's lock is what `up` waits on to bring the router
	// up. A page that was read a moment before another refresh wrote its own is
	// not newer than it; the page says when it was generated.
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return IndexSnapshot{}, err
	}
	if snapshot.DockerError != nil && !s.opts.QuietDocker {
		s.opts.Logger.Warn("The overview cannot show what runs because containers could not be listed: " + *snapshot.DockerError)
	}

	lk := lock.New(s.opts.Lock, lock.Options{Timeout: s.opts.LockTimeout, Holder: s.opts.Invocation, Logger: s.opts.Logger})
	if _, err := lk.Acquire(ctx, s.opts.Invocation); err != nil {
		return IndexSnapshot{}, err
	}
	defer func() {
		if lk.Lost() {
			_ = lk.Abandon()
			return
		}
		_ = lk.Release()
	}()

	// The icons first: the page links them, so it must never be served while
	// one is missing. A failed write over an icon an earlier refresh left is
	// only a warning (the page still refreshes, with the older icon); with an
	// icon missing the page is left alone and the error returned.
	if err := ensureIcons(filepath.Dir(s.opts.IndexFile), s.opts.Logger, s.writeIcons); err != nil {
		return IndexSnapshot{}, err
	}
	// The snapshot was read before the lock, so a refresh that read earlier can
	// get here after one that read later has written: the newer page stays.
	if written, ok := pageGeneratedAt(s.opts.IndexFile); ok && written > snapshot.GeneratedAt {
		return snapshot, nil
	}
	if err := files.AtomicWrite(s.opts.IndexFile, []byte(RenderIndexHTML(snapshot)), 0o644); err != nil {
		return IndexSnapshot{}, err
	}
	return snapshot, nil
}

// pageHeadLimit is how much of the page pageGeneratedAt reads: its header,
// after the styles, is well within it.
const pageHeadLimit = 64 * 1024

// generatedPattern finds the time the page says it was generated.
var generatedPattern = regexp.MustCompile(`<time datetime="([^"]*)"`)

// pageGeneratedAt is when the page at path says it was generated, from one
// bounded read of its head. ok is false for a page that is not there, is not a
// regular file or does not say, which a refresh then simply replaces. The file
// is opened without blocking, since a pipe with no writer would hold the router
// lock forever.
func pageGeneratedAt(path string) (generated string, ok bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	head, err := io.ReadAll(io.LimitReader(file, pageHeadLimit))
	if err != nil {
		return "", false
	}
	found := generatedPattern.FindSubmatch(head)
	if found == nil {
		return "", false
	}
	return string(found[1]), true
}

// RefreshBestEffort satisfies commands.Index: it regenerates the page,
// warning rather than failing the caller if it cannot. reason names what
// just happened ("up", "down", "worktree remove", ...), for the warning
// alone; nothing is printed when the refresh succeeds.
func (s *Service) RefreshBestEffort(ctx context.Context, reason string) {
	if _, err := s.Refresh(ctx); err != nil {
		s.opts.Logger.Warn(fmt.Sprintf("Could not refresh the overview after %s: %s", reason, err))
	}
}
