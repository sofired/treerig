// Package card is what a worktree publishes about itself for the
// machine-wide index. The registry record is deliberately minimal and
// crash-safe; the card is the human-facing complement, written by `up` into
// the worktree's own home and gone when the home goes. Anything the index
// cannot read is simply absent.
package card

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/stateformat"
)

// IndexCardFile is the card's filename inside a worktree's home.
const IndexCardFile = "index.json"

// maxCardBytes is generous for the shape below, and far short of anything
// that could hurt: a card is read by every operator on the machine, so an
// unbounded field would be an unbounded page.
const maxCardBytes = 128 * 1024

const maxTextLength = 512
const maxPathTextLength = 4096
const maxLabelNamespaceLength = 253
const maxURLLength = 2048
const maxServices = 64

// urlPattern is a superset of what worktreeUrls builds: RFC 3986 path
// characters plus the authority delimiters, so it excludes every control,
// bidi and homoglyph character along with quotes and angle brackets.
var urlPattern = regexp.MustCompile(`^https?://[A-Za-z0-9\-._~%!$&'()*+,;=:@/?#\[\]]+$`)

// IndexService is one routed service a card advertises on the shared index.
//
// Label is "" for a service the config never opted into `[service.<name>.index]`
// for: `up` still records its name so the index can count it — a stopped,
// unindexed service is exactly what should turn a Running row into
// Partial (#16) — but it never becomes a link, since there is no label or
// purpose to show for it. Purpose is meaningless without a Label and is
// always "" alongside one.
type IndexService struct {
	Name string
	// URL becomes an href on the index page; see urlPattern and the refusal
	// of userinfo in parseCardJSON.
	URL     string
	Label   string
	Purpose string
}

// IndexCard is what a worktree published about itself, last time `up` wrote it.
type IndexCard struct {
	Project string
	Domain  string
	Slug    string
	Host    string
	// Root is last seen; the index prefers live Git identity when it has it.
	Root   string
	Branch string
	// LabelNamespace is validated like the config field: it ends up inside
	// Go templates handed to `docker`.
	LabelNamespace string
	Services       []IndexService
	UpdatedAt      string
}

// IndexCardPath is where a worktree's card lives.
func IndexCardPath(controlHome, repositoryID, worktreeID string) string {
	return filepath.Join(layout.WorktreeLayout(controlHome, repositoryID, worktreeID).Home, IndexCardFile)
}

// Timestamp is now, as an ISO string to the millisecond with a trailing Z —
// what `up` records as a card's updatedAt, matching `new Date().toISOString()`.
func Timestamp(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000Z")
}

// cardServiceJSON is one routed service, in the schema's key order.
type cardServiceJSON struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
}

// cardJSON is the card as written, in the schema's key order:
// version, project, domain, slug, host, root, branch, labelNamespace,
// services[{name,url,label,purpose}], updatedAt.
type cardJSON struct {
	Version        int               `json:"version"`
	Project        string            `json:"project"`
	Domain         string            `json:"domain"`
	Slug           string            `json:"slug"`
	Host           string            `json:"host"`
	Root           string            `json:"root"`
	Branch         string            `json:"branch"`
	LabelNamespace string            `json:"labelNamespace"`
	Services       []cardServiceJSON `json:"services"`
	UpdatedAt      string            `json:"updatedAt"`
}

// WriteIndexCard writes card into the worktree's own home, which must
// already exist — `up` creates it before calling this. card is checked
// against cardBounds, the same bounds ReadIndexCardAt enforces on the way
// back in, so a card this package writes is always one it can read; a
// caller that manages to violate them (every field here comes from
// already-validated config and registration data) gets a plain error, not a
// *clierr.Error: this is an assertion on this package's own caller, never a
// user-facing failure.
func WriteIndexCard(controlHome, repositoryID, worktreeID string, card IndexCard) error {
	if err := cardBounds(card); err != nil {
		return err
	}
	services := make([]cardServiceJSON, len(card.Services))
	for i, service := range card.Services {
		services[i] = cardServiceJSON(service)
	}
	return files.AtomicJSON(IndexCardPath(controlHome, repositoryID, worktreeID), cardJSON{
		Version:        stateformat.IndexCard,
		Project:        card.Project,
		Domain:         card.Domain,
		Slug:           card.Slug,
		Host:           card.Host,
		Root:           card.Root,
		Branch:         card.Branch,
		LabelNamespace: card.LabelNamespace,
		Services:       services,
		UpdatedAt:      card.UpdatedAt,
	})
}

// cardBounds is the one definition of every bound a card must satisfy —
// length ranges, the label namespace pattern, the service count, and each
// service's own bounds and URL shape. WriteIndexCard checks a card against
// it before writing; parseCardJSON checks one against it after confirming
// every required key is present, on the way back in. Neither can drift
// from the other, because there is only the one definition.
func cardBounds(card IndexCard) error {
	if !validText(&card.Project, 1, maxTextLength) {
		return fmt.Errorf("card: project must be 1-%d characters", maxTextLength)
	}
	if !validText(&card.Domain, 1, maxTextLength) {
		return fmt.Errorf("card: domain must be 1-%d characters", maxTextLength)
	}
	if !validText(&card.Slug, 1, maxTextLength) {
		return fmt.Errorf("card: slug must be 1-%d characters", maxTextLength)
	}
	if !validText(&card.Host, 1, maxTextLength) {
		return fmt.Errorf("card: host must be 1-%d characters", maxTextLength)
	}
	if !validText(&card.Root, 0, maxPathTextLength) {
		return fmt.Errorf("card: root must be at most %d characters", maxPathTextLength)
	}
	if !validText(&card.Branch, 0, maxPathTextLength) {
		return fmt.Errorf("card: branch must be at most %d characters", maxPathTextLength)
	}
	if !validText(&card.LabelNamespace, 0, maxLabelNamespaceLength) || !brand.LabelNamespacePattern.MatchString(card.LabelNamespace) {
		return fmt.Errorf("card: labelNamespace %q is not a valid label namespace", card.LabelNamespace)
	}
	if !validText(&card.UpdatedAt, 0, maxTextLength) {
		return fmt.Errorf("card: updatedAt must be at most %d characters", maxTextLength)
	}
	if len(card.Services) > maxServices {
		return fmt.Errorf("card: at most %d services, got %d", maxServices, len(card.Services))
	}
	for _, service := range card.Services {
		if !validText(&service.Name, 1, maxTextLength) {
			return fmt.Errorf("card: service name must be 1-%d characters", maxTextLength)
		}
		// "" is a declared service the config never gave a [service.<name>.index]
		// block, not an absent field: see IndexService's own comment (#16).
		if !validText(&service.Label, 0, maxTextLength) {
			return fmt.Errorf("card: service label must be at most %d characters", maxTextLength)
		}
		if !validText(&service.Purpose, 0, maxTextLength) {
			return fmt.Errorf("card: service purpose must be at most %d characters", maxTextLength)
		}
		if !validURL(service.URL) {
			return fmt.Errorf("card: service url %q is not a valid routable URL", service.URL)
		}
	}
	return nil
}

// ReadIndexCard is ReadIndexCardAt for a worktree's usual home.
func ReadIndexCard(controlHome, repositoryID, worktreeID string) (*IndexCard, error) {
	return ReadIndexCardAt(IndexCardPath(controlHome, repositoryID, worktreeID))
}

// ReadIndexCardAt reads the card at file — a quarantined home, say. A
// genuinely absent file, an oversized one, or one that does not parse as a
// card all read as "no card": (nil, nil). Nothing about the file's own
// content ever fails this call, matching what a page built from many
// worktrees' cards needs — an unknown key or a URL past its bound is simply
// not a card, the same as one that was never written.
//
// (#18) A real I/O failure reading or stat-ing a file that does exist — most
// pointedly EMFILE, when thousands of registrations have exhausted this
// process's file descriptors — is different from either of those, and is
// reported as an error rather than folded into "no card": a worktree whose
// card could not be read is not the same as one that never published a card
// at all, and snapshot.go's caller renders the two differently (Unknown,
// not Missing or Stopped).
func ReadIndexCardAt(file string) (*IndexCard, error) {
	info, err := os.Stat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() > maxCardBytes {
		return nil, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			// Raced: gone between the Stat above and this Read (a
			// concurrent `worktree remove`, say). Still "no card", not a
			// failure — nothing is actually unreadable.
			return nil, nil
		}
		return nil, err
	}
	card, ok := parseCardJSON(data)
	if !ok {
		return nil, nil
	}
	return &card, nil
}

// Registration is the subset of a registry record RecordedLabelNamespace needs.
type Registration struct {
	RepositoryID   string
	WorktreeID     string
	LabelNamespace *string
}

// RecordedLabelNamespace is the label namespace a worktree's resources were
// recorded under: the registration says, or the index card does, wherever
// the home currently is. Undefined (nil) only for a worktree registered
// before either recorded it.
func RecordedLabelNamespace(controlHome string, worktree Registration) *string {
	if worktree.LabelNamespace != nil {
		return worktree.LabelNamespace
	}
	wt := layout.WorktreeLayout(controlHome, worktree.RepositoryID, worktree.WorktreeID)
	// Only the namespace, so a card the full schema refuses — an unknown
	// key, a field past its bound, a URL that is not one — still says which
	// labels its containers carry. Without it they go unlisted, and the row
	// reads Stopped while they are running.
	for _, home := range []string{wt.Home, wt.QuarantinePath} {
		if namespace := readLabelNamespace(filepath.Join(home, IndexCardFile)); namespace != nil {
			return namespace
		}
	}
	return nil
}

// rawIndexService is the on-disk shape of one service entry, decoded strictly.
type rawIndexService struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Label   *string `json:"label"`
	Purpose *string `json:"purpose"`
}

// rawIndexCard is the on-disk shape of a card, decoded strictly: an unknown
// key is a validation failure.
type rawIndexCard struct {
	Version        *float64          `json:"version"`
	Project        *string           `json:"project"`
	Domain         *string           `json:"domain"`
	Slug           *string           `json:"slug"`
	Host           *string           `json:"host"`
	Root           *string           `json:"root"`
	Branch         *string           `json:"branch"`
	LabelNamespace *string           `json:"labelNamespace"`
	Services       []rawIndexService `json:"services"`
	UpdatedAt      *string           `json:"updatedAt"`
}

// validText is cardBounds' one length check: min and max are in bytes, and
// (unlike a plain length comparison) it also refuses a field cardBounds is
// handed a nil pointer for — never true for WriteIndexCard, which starts
// from a value-typed IndexCard, but always meant to fail closed if that
// ever changes.
func validText(s *string, min, max int) bool {
	return s != nil && len(*s) >= min && len(*s) <= max
}

// validURL reports whether raw is short enough, matches urlPattern, parses,
// and carries no userinfo — cardBounds' check for every URL field.
func validURL(raw string) bool {
	if len(raw) > maxURLLength || !urlPattern.MatchString(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.User == nil
}

// requiredCardKeysPresent reports whether every key cardSchema requires is
// present (JSON decode leaves an absent or null key's pointer nil) —
// checked before cardBounds, which only ever sees a fully-populated
// IndexCard and so has no way to tell "absent" from "present but empty"
// apart on its own.
func requiredCardKeysPresent(raw rawIndexCard) bool {
	return raw.Project != nil && raw.Domain != nil && raw.Slug != nil && raw.Host != nil &&
		raw.Root != nil && raw.Branch != nil && raw.LabelNamespace != nil && raw.UpdatedAt != nil
}

// requiredServiceKeysPresent is requiredCardKeysPresent for one service entry.
func requiredServiceKeysPresent(s rawIndexService) bool {
	return s.Name != nil && s.URL != nil && s.Label != nil && s.Purpose != nil
}

// parseCardJSON decodes data as one index card: version 1, every required
// key present, and its bounds satisfied via cardBounds — the read side's
// half of the schema WriteIndexCard's own cardBounds call enforces on write.
//
// A card of any other version, a newer treerig's included, is not one this
// build can show, so it reads as no card. The next `up` regenerates a card
// and none decides anything, so the index leaves it out instead of refusing.
func parseCardJSON(data []byte) (IndexCard, bool) {
	var raw rawIndexCard
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return IndexCard{}, false
	}
	if raw.Version == nil || *raw.Version != stateformat.IndexCard {
		return IndexCard{}, false
	}
	if !requiredCardKeysPresent(raw) {
		return IndexCard{}, false
	}
	services := make([]IndexService, 0, len(raw.Services))
	for _, s := range raw.Services {
		if !requiredServiceKeysPresent(s) {
			return IndexCard{}, false
		}
		services = append(services, IndexService{Name: *s.Name, URL: *s.URL, Label: *s.Label, Purpose: *s.Purpose})
	}

	card := IndexCard{
		Project: *raw.Project, Domain: *raw.Domain, Slug: *raw.Slug, Host: *raw.Host,
		Root: *raw.Root, Branch: *raw.Branch, LabelNamespace: *raw.LabelNamespace,
		Services: services, UpdatedAt: *raw.UpdatedAt,
	}
	if cardBounds(card) != nil {
		return IndexCard{}, false
	}
	return card, true
}

// rawNamespaceOnly is deliberately not `.strict()`: a card the full schema
// refuses still names its labels, provided this one field is itself valid.
type rawNamespaceOnly struct {
	LabelNamespace *string `json:"labelNamespace"`
}

// readLabelNamespace is the one field a card is still useful for when the
// rest of it does not parse.
func readLabelNamespace(file string) *string {
	info, err := os.Stat(file)
	if err != nil || info.Size() > maxCardBytes {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var raw rawNamespaceOnly
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	if !validText(raw.LabelNamespace, 0, maxLabelNamespaceLength) || !brand.LabelNamespacePattern.MatchString(*raw.LabelNamespace) {
		return nil
	}
	return raw.LabelNamespace
}

// Rehost points a worktree's card at a new slug and host, for `worktree
// rename`: it sets Slug and Host and moves every service URL that sits under
// the card's old host (or a subdomain of it) to the new one, keeping scheme,
// port, path and the rest. Without it the index would drop each service,
// since it shows only URLs under the registered host. It reports whether it
// rewrote the card. A worktree that never published one, or whose card is
// unreadable as a card, has nothing to move: its next `up` writes a fresh
// one. Idempotent, so a rename interrupted after it ran can be run again.
func Rehost(controlHome, repositoryID, worktreeID, slug, host string) (bool, error) {
	current, err := ReadIndexCard(controlHome, repositoryID, worktreeID)
	if err != nil || current == nil {
		return false, err
	}
	previous := current.Host
	moved := *current
	moved.Slug = slug
	moved.Host = host
	moved.Services = make([]IndexService, len(current.Services))
	for i, service := range current.Services {
		service.URL = rehostURL(service.URL, previous, host)
		moved.Services[i] = service
	}
	if moved.Slug == current.Slug && moved.Host == current.Host && slices.Equal(moved.Services, current.Services) {
		return false, nil
	}
	if err := WriteIndexCard(controlHome, repositoryID, worktreeID, moved); err != nil {
		return false, err
	}
	return true, nil
}

// rehostURL is raw with its hostname moved from oldHost (or a subdomain of
// it) to newHost, and raw itself when it names another host or does not parse.
func rehostURL(raw, oldHost, newHost string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	name := parsed.Hostname()
	switch {
	case name == oldHost:
		name = newHost
	case strings.HasSuffix(name, "."+oldHost):
		name = strings.TrimSuffix(name, oldHost) + newHost
	default:
		return raw
	}
	if port := parsed.Port(); port != "" {
		name += ":" + port
	}
	parsed.Host = name
	return parsed.String()
}
