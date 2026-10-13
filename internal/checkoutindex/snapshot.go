// Package checkoutindex builds the checkout index: registry records + cards
// + Git identities + a container inventory in, one sorted snapshot out, and
// the HTML and plain-text pages rendered from it. snapshot.go and render.go
// are pure, so every state a row can be in is reachable from a unit test;
// service.go is what runs the `docker ps` call that produces a
// []ContainerRecord and ties this package to disk and the router lock
// (#41).
package checkoutindex

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/text"
)

// IndexStatus is a row's state, as the legend on the page explains it.
type IndexStatus string

const (
	StatusRunning IndexStatus = "running"
	StatusPartial IndexStatus = "partial"
	StatusStopped IndexStatus = "stopped"
	StatusMissing IndexStatus = "missing"
	StatusUnknown IndexStatus = "unknown"
)

// IndexServiceLink is one routed service a row links to.
type IndexServiceLink struct {
	Name      string
	Label     string
	Purpose   string
	URL       string
	Available bool
}

// IndexSharedService is one routed web UI of a repository's shared services,
// listed in a group of its own beneath the checkouts (docs/design/routing.md,
// decision 20). It is read from the shared-service records, so it is listed
// whether or not its container runs.
type IndexSharedService struct {
	RepositoryID string
	// Name is the shared service's name.
	Name string
	// Project is the name of a project of the repository, from one of its
	// checkouts' cards; the domain when none has named it.
	Project string
	Domain  string
	// Host is <name>.shared.<domain>.
	Host string
	// URL is the address the proxy answers on, router port included.
	URL string
	// Available means a container of the service is running and not
	// reporting itself unhealthy or still starting.
	Available bool
}

// IndexEntry is one row of the checkout index.
type IndexEntry struct {
	RepositoryID string
	WorktreeID   string
	Project      string
	Domain       string
	Slug         string
	Host         string
	// Branch is the branch checked out; nil when nothing knows (no live Git
	// identity and no card), and a pointer to "" for a detached HEAD — an
	// answer in its own right, distinguishing "absent" from "empty".
	Branch *string
	// Root is last seen; empty and absent mean the same thing here, since a
	// root is a path, so it needs no pointer the way Branch does.
	Root     string
	Status   IndexStatus
	Services []IndexServiceLink
}

// IdentityFailure is what it cost the page that Git could not be asked which
// worktrees of a repository exist. It is an output type: it appears on
// IndexSnapshot, for render.go to read, but never on SnapshotInput — see
// IdentityFailureInput below for why, and TestSnapshotInputIdentityErrorHasNoRowsUnknown
// for what pins it.
type IdentityFailure struct {
	Reason string
	// RowsUnknown is true when the repository was identified and only its
	// worktree list failed, so its own rows are Unknown rather than Missing.
	// False when the repository itself could not be identified: then no row
	// on the page is its own, and none of them was going to be called
	// missing anyway.
	RowsUnknown bool
}

// IdentityFailureInput is what a SnapshotInput may say about a failed
// identity lookup: Reason alone.
//
// (#17) IndexSnapshot's own IdentityFailure carries RowsUnknown too, but
// that is BuildSnapshot's own answer to give, not a caller's: whether it is
// true depends entirely on whether the repository itself was identified,
// which SnapshotInput already says on its own (CurrentRepositoryID). Giving
// the input type a second, independently-settable RowsUnknown that was
// merely supposed to always agree with that is exactly what once let a
// caller build a SnapshotInput naming a known CurrentRepositoryID together
// with RowsUnknown: false — a combination entryStatus's own logic read as
// "this repository is not the local one", marking every local row Missing
// instead of Unknown. The issue's own done-when asked for this to fail to
// type-check, not merely to run correctly: IdentityFailureInput has no
// RowsUnknown field at all, so the combination cannot be constructed, not
// just "is recomputed if you get it wrong."
type IdentityFailureInput struct {
	Reason string
}

// IndexSnapshot is what one render of the page is built from.
type IndexSnapshot struct {
	Entries []IndexEntry
	// Shared are the repositories' routed shared-service UIs, sorted by
	// domain, project and name.
	Shared      []IndexSharedService
	GeneratedAt string
	// IdentityError is set when Git could not say which worktrees of the
	// current repository exist.
	IdentityError *IdentityFailure
	// DockerReachable is false when Docker could not be asked, so no row's
	// services could be checked.
	DockerReachable bool
	// DockerError says why, when it could not be: the page is read by
	// people who did not run the command.
	DockerError *string
	// CardReadFailures (#18) is how many registrations' index cards could
	// not be read for a reason other than the card never having been
	// written — see SnapshotInput's own field. Zero means every card that
	// exists was read; render.go shows a notice whenever it is not.
	CardReadFailures int
}

// ContainerRecord is one managed container, as `docker ps` reports it.
type ContainerRecord struct {
	RepositoryID string
	// WorktreeID is empty for a container of a repository's shared services,
	// which belong to no worktree (Shared is then true).
	WorktreeID string
	Service    string
	// Shared marks a container of a repository's shared services.
	Shared bool
	// Available means running, and not reporting itself unhealthy or still
	// starting.
	Available bool
}

// unhealthyPattern matches a `docker ps` status announcing a container is
// not ready, case-insensitively, wherever it appears in the status text.
var unhealthyPattern = regexp.MustCompile(`(?i)\((?:unhealthy|health: starting)\)`)

// ParseContainerInventory parses the tab-separated `docker ps` format the
// index service asks for: state, status, repository, worktree, service, and
// the shared label ("true" for a container of a repository's shared
// services, which carries no worktree label). It never returns a nil slice on success — even
// for empty output, which means Docker answered and named no containers —
// since SnapshotInput.Containers uses nil specifically to mean Docker could
// not be asked at all, and service.go's caller may pass this return value
// straight through or merge several of them together.
func ParseContainerInventory(output string) ([]ContainerRecord, error) {
	records := make([]ContainerRecord, 0, strings.Count(output, "\n")+1)
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			return nil, clierr.New("Docker returned an unrecognized container inventory line.")
		}
		state, status, repositoryID, worktreeID, service, sharedLabel := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]
		isShared := sharedLabel == "true"
		// The shared field is exactly "true" or empty: anything else is a
		// tab inside an earlier field that shifted the rest.
		if state == "" || repositoryID == "" || service == "" || (sharedLabel != "" && !isShared) || (worktreeID == "") != isShared {
			return nil, clierr.New("Docker returned an unrecognized container inventory line.")
		}
		records = append(records, ContainerRecord{
			RepositoryID: repositoryID,
			WorktreeID:   worktreeID,
			Service:      service,
			Shared:       isShared,
			Available:    state == "running" && !unhealthyPattern.MatchString(status),
		})
	}
	return records, nil
}

// SnapshotInput is everything BuildSnapshot needs.
type SnapshotInput struct {
	Registrations []registry.RegisteredWorktree
	// SharedRecords are the shared-service records of the control home;
	// those that route a web UI are listed.
	SharedRecords []registry.SharedRecord
	// Cards is keyed by EntryKey.
	Cards map[string]card.IndexCard
	// Identities are the live worktrees of the repository the command runs in.
	Identities []registry.WorktreeIdentity
	// CurrentRepositoryID is nil when the command could not identify its
	// own repository.
	CurrentRepositoryID *string
	// Containers is nil when Docker could not be asked; a non-nil, possibly
	// empty, slice once it could.
	Containers []ContainerRecord
	// DockerError is what Docker said when it could not be asked; shown to
	// whoever reads the page.
	DockerError *string
	// RouterPort is the port the router publishes; a service URL on any
	// other port is not this checkout's.
	RouterPort int
	// IdentityError is set when Git could not be asked about this
	// repository. Its type carries only a Reason (#17): whether the rows it
	// affects are Unknown rather than Missing is for BuildSnapshot alone to
	// decide, from CurrentRepositoryID — see IdentityFailureInput.
	IdentityError *IdentityFailureInput
	// CardReadFailures (#18) is keyed by EntryKey, for a registration whose
	// card could not be read for a reason other than it never having been
	// written — most pointedly EMFILE, once thousands of registrations have
	// exhausted this process's file descriptors. Distinguishing this from
	// "no card" matters: a checkout that has never come up correctly reads
	// Stopped or Missing, but one whose card merely could not be opened this
	// time must not be reported with the same confidence — see entryStatus.
	CardReadFailures map[string]struct{}
	GeneratedAt      string
}

// EntryKey is the map key a registration's card and identity are found
// under: repositoryId:worktreeId.
func EntryKey(repositoryID, worktreeID string) string {
	return repositoryID + ":" + worktreeID
}

// BuildSnapshot is pure: registry records + cards + Git identities + a
// container inventory in, one sorted snapshot out.
func BuildSnapshot(input SnapshotInput) IndexSnapshot {
	identities := make(map[string]registry.WorktreeIdentity, len(input.Identities))
	for _, identity := range input.Identities {
		identities[EntryKey(identity.RepositoryID, identity.WorktreeID)] = identity
	}
	dockerReachable := input.Containers != nil

	// (#17) RowsUnknown is BuildSnapshot's own answer, computed here from
	// CurrentRepositoryID — the one fact that actually decides it — since
	// IdentityFailureInput carries no such field for a caller to set at all.
	var identityError *IdentityFailure
	if input.IdentityError != nil {
		identityError = &IdentityFailure{
			Reason:      input.IdentityError.Reason,
			RowsUnknown: input.CurrentRepositoryID != nil,
		}
	}

	entries := make([]IndexEntry, len(input.Registrations))
	for i, record := range input.Registrations {
		key := EntryKey(record.RepositoryID, record.WorktreeID)
		indexCard, hasCard := input.Cards[key]
		identity, hasIdentity := identities[key]
		local := input.CurrentRepositoryID != nil && record.RepositoryID == *input.CurrentRepositoryID

		var owned []ContainerRecord
		if dockerReachable {
			for _, container := range input.Containers {
				if container.RepositoryID == record.RepositoryID && container.WorktreeID == record.WorktreeID {
					owned = append(owned, container)
				}
			}
		}

		host := layout.WorktreeHost(record.Slug, record.Domain)
		// A card is written into a home every lifecycle hook is handed, so
		// where a service lives is not something it gets to say: a link
		// that is not under this checkout's own host is dropped rather than
		// published to everyone.
		var declared []card.IndexService
		if hasCard {
			for _, service := range indexCard.Services {
				if serviceBelongsToHost(service.URL, host, input.RouterPort) {
					declared = append(declared, service)
				}
			}
		}

		_, cardReadFailed := input.CardReadFailures[key]
		status := entryStatus(local, hasIdentity, dockerReachable, cardReadFailed, identityError, declared, owned)

		// (#16) declared is every service the config declared for this
		// checkout, whether or not it opted into [service.<name>.index] — see
		// entryStatus below and card.IndexService's own comment — but only
		// the ones that did get a Label are shown as a link: an unlabelled
		// entry has no href text or purpose to publish, only a name that
		// still needs counting toward the row's status.
		services := make([]IndexServiceLink, 0, len(declared))
		for _, service := range declared {
			if service.Label == "" {
				continue
			}
			services = append(services, IndexServiceLink{
				Name: service.Name, Label: service.Label, Purpose: service.Purpose, URL: service.URL,
				Available: status != StatusMissing && anyContainerAvailable(owned, service.Name),
			})
		}

		services = append(services, hostRouteLinks(record, input.RouterPort, status)...)

		// A pointer, not `||`: a detached HEAD reports an empty branch,
		// which is an answer, and must not fall through to the card's stale
		// one.
		var branch *string
		switch {
		case hasIdentity:
			b := identity.Branch
			branch = &b
		case hasCard:
			b := indexCard.Branch
			branch = &b
		}
		// A root is a path, so empty and absent mean the same thing here; a
		// branch is not, which is why the switch above has to distinguish them.
		var root string
		switch {
		case hasIdentity:
			root = identity.Root
		case hasCard:
			root = indexCard.Root
		}

		project := record.Domain
		if hasCard {
			project = indexCard.Project
		}

		entries[i] = IndexEntry{
			RepositoryID: record.RepositoryID,
			WorktreeID:   record.WorktreeID,
			Project:      project,
			Domain:       record.Domain,
			Slug:         record.Slug,
			Host:         host,
			Branch:       branch,
			Root:         root,
			Status:       status,
			Services:     services,
		}
	}
	// Sorted on the key the page groups on, so a table's rows cannot
	// interleave with another's. A fixed locale (text.Collate), so two
	// machines given the same input write the same page.
	sortEntries(entries)

	return IndexSnapshot{
		Entries:          entries,
		Shared:           sharedServices(input, entries, dockerReachable),
		GeneratedAt:      input.GeneratedAt,
		IdentityError:    identityError,
		DockerReachable:  dockerReachable,
		DockerError:      input.DockerError,
		CardReadFailures: len(input.CardReadFailures),
	}
}

// sharedServices lists the routed web UIs among the shared-service records,
// each with the project name one of its repository's checkouts gave, and
// whether a container of it answers. A record that routes nothing is not
// listed. Sorted on domain, project, then name, with the same fixed locale
// as the checkouts.
func sharedServices(input SnapshotInput, entries []IndexEntry, dockerReachable bool) []IndexSharedService {
	var listed []IndexSharedService
	for _, record := range input.SharedRecords {
		host := record.Host()
		if host == "" {
			continue
		}
		project := record.Domain
		for _, entry := range entries {
			if entry.RepositoryID == record.RepositoryID {
				project = entry.Project
				break
			}
		}
		suffix := ""
		if input.RouterPort != 80 {
			suffix = ":" + strconv.Itoa(input.RouterPort)
		}
		available := false
		if dockerReachable {
			for _, container := range input.Containers {
				available = available || (container.Shared && container.RepositoryID == record.RepositoryID &&
					container.Service == record.Name && container.Available)
			}
		}
		listed = append(listed, IndexSharedService{
			RepositoryID: record.RepositoryID, Name: record.Name, Project: project, Domain: record.Domain,
			Host: host, URL: "http://" + host + suffix, Available: available,
		})
	}
	slices.SortStableFunc(listed, func(left, right IndexSharedService) int {
		if c := text.Collate(left.Domain, right.Domain); c != 0 {
			return c
		}
		if c := text.Collate(left.Project, right.Project); c != 0 {
			return c
		}
		if c := text.Collate(left.Name, right.Name); c != 0 {
			return c
		}
		return text.Collate(left.RepositoryID, right.RepositoryID)
	})
	return listed
}

// entryStatus mirrors buildSnapshot's status derivation exactly.
func entryStatus(local, hasIdentity, dockerReachable, cardReadFailed bool, identityError *IdentityFailure, declared []card.IndexService, owned []ContainerRecord) IndexStatus {
	switch {
	// (#18) Checked first: a card that could not be read is not evidence of
	// anything about the checkout itself, so it must outrank every other
	// answer below, including a live Git identity that would otherwise call
	// it Running or Partial from containers alone.
	case cardReadFailed:
		return StatusUnknown
	case local && identityError != nil && identityError.RowsUnknown:
		return StatusUnknown
	case local && !hasIdentity:
		return StatusMissing
	case !dockerReachable:
		return StatusUnknown
	}
	// Counted per service the card names — every service the config
	// declares, whether or not it also opted into [service.<name>.index] (#16) —
	// union the containers that exist. A service that never got a container
	// at all is exactly the one a `Running` pill would lie about, and that
	// is as true of one with no [service.<name>.index] block as one with: a
	// stopped, unindexed service still keeps its checkout from reading
	// Running. A checkout that names none and has none reads Stopped, which
	// is what a registration that has never been up is.
	names := make(map[string]struct{}, len(declared)+len(owned))
	for _, service := range declared {
		names[service.Name] = struct{}{}
	}
	for _, container := range owned {
		names[container.Service] = struct{}{}
	}
	if len(names) == 0 {
		return StatusStopped
	}
	// Every replica of a service carries the same Compose label. A service
	// answers only when all of its containers do — one healthy replica out
	// of three is not "every service answers" — but one that answers at all
	// is not "none do" either, which is what the legend promises Stopped
	// means.
	allAnswer, anyAnswers := true, false
	for name := range names {
		containers := containersFor(owned, name)
		all := len(containers) > 0
		any := false
		for _, container := range containers {
			if container.Available {
				any = true
			} else {
				all = false
			}
		}
		if !all {
			allAnswer = false
		}
		if any {
			anyAnswers = true
		}
	}
	switch {
	case allAnswer:
		return StatusRunning
	case anyAnswers:
		return StatusPartial
	default:
		return StatusStopped
	}
}

// containersFor is every container among owned carrying the named service,
// which may be more than one for a scaled service.
func containersFor(owned []ContainerRecord, service string) []ContainerRecord {
	var containers []ContainerRecord
	for _, container := range owned {
		if container.Service == service {
			containers = append(containers, container)
		}
	}
	return containers
}

// anyContainerAvailable reports whether any container among owned carrying
// the named service is available.
func anyContainerAvailable(owned []ContainerRecord, service string) bool {
	for _, container := range owned {
		if container.Service == service && container.Available {
			return true
		}
	}
	return false
}

// serviceBelongsToHost is buildSnapshot's URL filter: `http:` on the
// router's own port, under this checkout's own host (or a subdomain of it).
// Card.go's own urlPattern already restricts a card's URL to a fixed ASCII
// character set with no userinfo before it ever reaches here; this filter
// runs anyway, since buildSnapshot is also exercised directly with cards
// built in memory that skip that validation.
//
// This filter is meant to match the WHATWG `URL.parse` standard, which
// normalizes several things Go's net/url does not;
// internal/checkoutindex/testdata/url-filter.json, a hand-maintained
// fixture recorded through the real buildSnapshot, is what proves each of
// the following, rather than a comment asserting it:
//   - scheme is lower-cased ("HTTP:" -> "http:"): net/url already does this,
//     since url.Parse's Scheme field is normalized to lower case.
//   - hostname is lower-cased ("Feature-A.Acme" -> "feature-a.acme"): net/url
//     does not — Hostname() returns the string as written — so it is
//     lower-cased explicitly below.
//   - a port equal to the scheme's default (":80" for http) reads as ""
//     (empty): net/url's Port() keeps the literal "80" instead, but since
//     the comparison below treats an empty port the same as 80, an explicit
//     ":80" and an omitted port compare equal either way — no code
//     difference needed.
//   - a port with a leading zero (":065") is renormalized to its plain
//     decimal form ("65"): strconv.Atoi parses "065" as decimal 65 as well
//     (it never treats a leading zero as octal), so the two already agree
//     numerically.
//   - a trailing dot on the hostname ("host.") is kept as written by both,
//     so a card URL with one fails the host comparison identically in both
//     builds (a registry host never ends in a dot).
//   - an IPv6 literal's brackets ("[::1]") are kept in the WHATWG hostname
//     but stripped by net/url's Hostname(); left alone, since neither value
//     can ever equal or be a subdomain of a registry host, which is always
//     a DNS name.
//   - a non-ASCII hostname would be IDNA-encoded by WHATWG but is left as
//     written by net/url; not reproduced, since urlPattern above refuses
//     any non-ASCII byte in a URL before one reaches a real card.
func serviceBelongsToHost(rawURL, host string, routerPort int) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	port := 80
	if p := parsed.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		port = n
	}
	// The router's own port, too: every worktree host is loopback, so
	// another port is another listener on this machine entirely.
	if port != routerPort {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return hostname == host || strings.HasSuffix(hostname, "."+host)
}

// sortEntries orders entries by domain, project, missing last, branch (or
// slug when there is none), then slug — a total order, so it never falls
// back to whatever order the registry happened to be read in. Stable, so
// entries that compare equal keep their original relative order.
func sortEntries(entries []IndexEntry) {
	slices.SortStableFunc(entries, func(left, right IndexEntry) int {
		if c := text.Collate(left.Domain, right.Domain); c != 0 {
			return c
		}
		// Two projects may share a domain, and so share a table: keep each
		// one's rows together, with its gone checkouts after its live ones.
		if c := text.Collate(left.Project, right.Project); c != 0 {
			return c
		}
		if c := missingRank(left.Status) - missingRank(right.Status); c != 0 {
			return c
		}
		if c := text.Collate(sortKey(left), sortKey(right)); c != 0 {
			return c
		}
		return text.Collate(left.Slug, right.Slug)
	})
}

// missingRank is 1 for a missing entry and 0 otherwise, so subtracting one
// rank from another sorts missing entries last.
func missingRank(status IndexStatus) int {
	if status == StatusMissing {
		return 1
	}
	return 0
}

// sortKey is `left.branch || left.slug`: a detached checkout has no branch
// to sort by, so it sorts by the slug that names it instead of sorting
// ahead of everything.
func sortKey(entry IndexEntry) string {
	if entry.Branch != nil && *entry.Branch != "" {
		return *entry.Branch
	}
	return entry.Slug
}

// PlaceholderSnapshot is the page to write before anything has been read: no
// checkouts, and Docker not asked. DockerReachable false is the honest
// value — the router writes this while bringing itself up, long before a
// container has been listed.
func PlaceholderSnapshot(generatedAt string) IndexSnapshot {
	return IndexSnapshot{GeneratedAt: generatedAt}
}

// DescribedBranch is what a row says it is on: the branch, a detached HEAD,
// or that nothing has recorded one.
type DescribedBranch struct {
	Text  string
	Muted bool
}

// DescribeBranch describes a checkout's branch for display. The two
// synthesized answers are parenthesized because "detached" and "unknown"
// are both legal branch names — parentheses, not styling, are what tells a
// placeholder from a ref.
func DescribeBranch(branch *string) DescribedBranch {
	if branch == nil {
		return DescribedBranch{Text: "(no branch)", Muted: true}
	}
	if *branch == "" {
		return DescribedBranch{Text: "(detached)", Muted: true}
	}
	return DescribedBranch{Text: *branch, Muted: false}
}
