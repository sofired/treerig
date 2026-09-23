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
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
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
// against the same bounds ReadIndexCardAt enforces on the way back in, so a
// card this package writes is always one it can read; a caller that manages
// to violate them (every field here comes from already-validated config and
// registration data) gets a plain error, not a *clierr.Error: like the
// TypeScript build's zod parse, this is an assertion on this package's own
// caller, never a user-facing failure.
func WriteIndexCard(controlHome, repositoryID, worktreeID string, card IndexCard) error {
	if err := validateForWrite(card); err != nil {
		return err
	}
	services := make([]cardServiceJSON, len(card.Services))
	for i, service := range card.Services {
		services[i] = cardServiceJSON{Name: service.Name, URL: service.URL, Label: service.Label, Purpose: service.Purpose}
	}
	return files.AtomicJSON(IndexCardPath(controlHome, repositoryID, worktreeID), cardJSON{
		Version:        1,
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

// validateForWrite applies exactly the bounds ReadIndexCardAt checks coming
// back in (validText, validURL, brand.LabelNamespacePattern), so a card this
// package writes is always one it can read.
func validateForWrite(card IndexCard) error {
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
		if !validText(&service.Label, 1, maxTextLength) {
			return fmt.Errorf("card: service label must be 1-%d characters", maxTextLength)
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
func ReadIndexCard(controlHome, repositoryID, worktreeID string) *IndexCard {
	return ReadIndexCardAt(IndexCardPath(controlHome, repositoryID, worktreeID))
}

// ReadIndexCardAt reads the card at file — a quarantined home, say. Missing,
// unreadable, oversized, or unrecognized all read as "no card": nothing here
// ever fails, matching what a page built from many worktrees' cards needs.
func ReadIndexCardAt(file string) *IndexCard {
	info, err := os.Stat(file)
	if err != nil || info.Size() > maxCardBytes {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	card, ok := parseCardJSON(data)
	if !ok {
		return nil
	}
	return &card
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
// key is a validation failure, exactly as the TypeScript schema's
// `.strict()` treats it.
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

func validText(s *string, min, max int) bool {
	return s != nil && len(*s) >= min && len(*s) <= max
}

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

func parseCardJSON(data []byte) (IndexCard, bool) {
	var raw rawIndexCard
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return IndexCard{}, false
	}

	if raw.Version == nil || *raw.Version != 1 {
		return IndexCard{}, false
	}
	if !validText(raw.Project, 1, maxTextLength) || !validText(raw.Domain, 1, maxTextLength) ||
		!validText(raw.Slug, 1, maxTextLength) || !validText(raw.Host, 1, maxTextLength) {
		return IndexCard{}, false
	}
	if !validText(raw.Root, 0, maxPathTextLength) || !validText(raw.Branch, 0, maxPathTextLength) {
		return IndexCard{}, false
	}
	if !validText(raw.LabelNamespace, 0, maxLabelNamespaceLength) || !brand.LabelNamespacePattern.MatchString(*raw.LabelNamespace) {
		return IndexCard{}, false
	}
	if !validText(raw.UpdatedAt, 0, maxTextLength) {
		return IndexCard{}, false
	}
	if len(raw.Services) > maxServices {
		return IndexCard{}, false
	}
	services := make([]IndexService, 0, len(raw.Services))
	for _, s := range raw.Services {
		if !validText(s.Name, 1, maxTextLength) || !validText(s.Label, 1, maxTextLength) || !validText(s.Purpose, 0, maxTextLength) {
			return IndexCard{}, false
		}
		if s.URL == nil || !validURL(*s.URL) {
			return IndexCard{}, false
		}
		services = append(services, IndexService{Name: *s.Name, URL: *s.URL, Label: *s.Label, Purpose: *s.Purpose})
	}

	return IndexCard{
		Project: *raw.Project, Domain: *raw.Domain, Slug: *raw.Slug, Host: *raw.Host,
		Root: *raw.Root, Branch: *raw.Branch, LabelNamespace: *raw.LabelNamespace,
		Services: services, UpdatedAt: *raw.UpdatedAt,
	}, true
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
