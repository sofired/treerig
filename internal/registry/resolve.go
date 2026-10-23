package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/text"
)

// ErrNoWorktree is what errors.Is reports for a name that matches no
// registered worktree, so a caller with another place to look (`worktree
// unlock` reads the locks a worktree that never registered left behind) can
// tell "nothing by that name" from "that name is ambiguous" or "a read failed".
var ErrNoWorktree = errors.New("no registered worktree has that name")

// NameOptions say how Resolve and NameCandidates read a name. The zero value
// reads a slug, a host or a UUID, and refuses a relative path (it needs Cwd).
type NameOptions struct {
	// Domain narrows a slug to the worktrees registered under it ("" means
	// every domain). It narrows only the slug rule: a UUID, a host and a path
	// each name one worktree already, and a host carries its domain. It exists
	// for `--domain`, which is removed with #211 and #213.
	Domain string
	// Cwd is the physical working directory a relative path form ("." or
	// "./x") is read from. The registry never asks the process for it.
	Cwd string
	// Home is the home directory a leading "~" stands for; "" means the
	// registry's own UserHome. The registry never reads the environment, so
	// the entry point's value arrives either way.
	Home string
	// RepositoryID is the current repository's identity: "" outside any
	// repository, or in one treerig has not identified. Nil asks Git, and only
	// when a slug is registered in more than one repository, since that is the
	// one case where it changes the answer.
	RepositoryID *string
	// WithinDirectory lets a path inside a worktree's directory, not only its
	// root, name that worktree. Only a read-only caller sets it: a destructive
	// or lock-clearing verb asks for the root, or another form of the name.
	WithinDirectory bool
}

// Resolved is the worktree a name resolved to, with the directory its index
// card last recorded, so a caller that needs both does not read the card again.
type Resolved struct {
	Record RegisteredWorktree
	// Root is the recorded directory, "" when the worktree was never brought up
	// or its card could not be read. A path form always has one: it matched it.
	Root string
}

// NameCandidate is one registered worktree as shell completion offers it.
type NameCandidate struct {
	// Name is the shortest form of the worktree's name that Resolve reads back
	// as this worktree alone: its slug, else its host, else its UUID.
	Name   string
	Record RegisteredWorktree
	// Root is the directory the worktree's index card last recorded, "" when it
	// was never brought up or its card could not be read.
	Root string
}

// uuidShape is the 36-character form of a UUID: 8-4-4-4-12 hexadecimal
// digits. It is looser than uuidPattern on purpose: a name that has the shape
// is read as a UUID whatever its version bits (it is the one 8-4-4-4-12 pattern:
// the commands package asks IsUUID too, and uuidPattern is the stricter form a
// stored record's own IDs must have), so a mistyped one says "no such
// worktree" instead of falling through to the slug rule.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether name is a worktree UUID in its 36-character form.
func IsUUID(name string) bool {
	return uuidShape.MatchString(name)
}

// IsPathName reports whether name is written as a path: "." or "..", or a
// name that starts with "/", "./", "../" or "~". Shell completion's
// isPathPrefix (internal/cli) is the prefix of this: keep the two in step.
// Anything else is never read
// as a directory, so a directory called feature-x cannot change what
// 'treerig worktree remove feature-x' means.
func IsPathName(name string) bool {
	switch name {
	case ".", "..":
		return true
	}
	return strings.HasPrefix(name, "/") || strings.HasPrefix(name, "./") ||
		strings.HasPrefix(name, "../") || strings.HasPrefix(name, "~")
}

// Resolve reads name as the worktree it names, or an error that says why it
// names none. The forms cannot overlap, so the order they are tried in does not
// change an answer:
//
//   - a UUID (36 characters, hexadecimal) names that worktree;
//   - a path form ("." or "..", or a name starting with "/", "./", "../" or "~")
//     names the worktree whose recorded directory is its longest match, after
//     both are resolved through symbolic links. A path inside a directory, not
//     at its root, is accepted only with opts.WithinDirectory;
//   - a name with a dot is a host, which names the registered worktree with
//     exactly that host (compared without regard to case);
//   - anything else is a slug (a hostname label has no dot): this
//     repository's first, else it must be unique on the machine.
//
// A name that matches nothing fails with an error for which
// errors.Is(err, ErrNoWorktree) holds. A name that matches several lists their
// hosts and UUIDs. It never guesses and never reads a bare word as a
// directory. Read-only: it takes no lock and writes nothing.
func (r *Registry) Resolve(ctx context.Context, name string, opts NameOptions) (*Resolved, error) {
	if name == "" {
		return nil, errors.New("registry: Resolve needs a name; the caller decides what no name means")
	}
	scan, err := ScanRegistrations(r.ControlHome)
	if err != nil {
		return nil, err
	}
	switch {
	case IsUUID(name):
		return r.resolveUUID(scan.Records, name)
	case IsPathName(name):
		return r.resolvePath(scan.Records, name, opts)
	}
	lower := strings.ToLower(name)
	var byHost []RegisteredWorktree
	for _, record := range scan.Records {
		if strings.ToLower(layout.WorktreeHost(record.Slug, record.Domain)) == lower {
			byHost = append(byHost, record)
		}
	}
	if len(byHost) > 0 {
		return r.single(name, byHost, "")
	}
	return r.resolveSlug(ctx, scan.Records, name, opts)
}

// resolved is record with the directory its card recorded, read best-effort:
// a card that cannot be read leaves Root empty rather than failing a lookup
// that did not need it.
func (r *Registry) resolved(record RegisteredWorktree) *Resolved {
	root := ""
	if seen, err := card.ReadIndexCard(r.ControlHome, record.RepositoryID, record.WorktreeID); err == nil && seen != nil && usableRoot(seen.Root) {
		root = seen.Root
	}
	return &Resolved{Record: record, Root: root}
}

// resolveUUID is the registered worktree with this UUID, which is unique on
// the machine; the spelling's case is not significant.
func (r *Registry) resolveUUID(records []RegisteredWorktree, name string) (*Resolved, error) {
	want := strings.ToLower(name)
	for _, record := range records {
		if strings.ToLower(record.WorktreeID) == want {
			return r.resolved(record), nil
		}
	}
	return nil, noWorktree("No worktree is registered as %s.", text.Printable(name))
}

// resolveSlug is the slug rule: the registered worktrees with this slug,
// narrowed to opts.Domain, then this repository's own when they span more
// than one.
func (r *Registry) resolveSlug(ctx context.Context, records []RegisteredWorktree, name string, opts NameOptions) (*Resolved, error) {
	var matches []RegisteredWorktree
	for _, record := range records {
		if record.Slug == name && (opts.Domain == "" || record.Domain == opts.Domain) {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		where := ""
		if opts.Domain != "" {
			where = " under " + text.Printable(opts.Domain)
		}
		return nil, noWorktree("No worktree is registered as %s%s.", text.Printable(name), where)
	}
	hint := ""
	if opts.Domain == "" {
		hint = ", or add --domain"
	}
	return r.single(name, r.narrowToRepository(ctx, matches, opts), hint)
}

// single is the one record in matches, or an error listing them all. hint is
// appended to the advice on how to name one.
func (r *Registry) single(name string, matches []RegisteredWorktree, hint string) (*Resolved, error) {
	if len(matches) == 1 {
		return r.resolved(matches[0]), nil
	}
	return nil, r.ambiguous(fmt.Sprintf("'%s' names more than one worktree", text.Printable(name)), matches, hint)
}

// ambiguous is the refusal that lists every candidate's host and UUID, so the
// next command can name one of them.
func (r *Registry) ambiguous(subject string, matches []RegisteredWorktree, hint string) error {
	listed := make([]string, len(matches))
	for i, record := range matches {
		listed[i] = fmt.Sprintf("%s (%s)", text.Printable(layout.WorktreeHost(record.Slug, record.Domain)), record.WorktreeID)
	}
	sort.Strings(listed)
	return clierr.Newf("%s: %s. Name one by its host or its UUID%s.", subject, strings.Join(listed, ", "), hint)
}

// narrowToRepository prefers the current repository's own worktrees among
// matches registered in more than one repository. It asks Git which
// repository that is only then, so a name that is unique anyway costs no
// subprocess. When Git cannot say, the narrowing is skipped with a warning
// and the caller sees every match, which is the safe side of a destructive
// verb.
func (r *Registry) narrowToRepository(ctx context.Context, matches []RegisteredWorktree, opts NameOptions) []RegisteredWorktree {
	if len(matches) < 2 || !spansRepositories(matches) {
		return matches
	}
	id := r.currentRepositoryID(ctx, opts)
	if id == "" {
		return matches
	}
	var mine []RegisteredWorktree
	for _, record := range matches {
		if record.RepositoryID == id {
			mine = append(mine, record)
		}
	}
	if len(mine) == 0 {
		return matches
	}
	return mine
}

// spansRepositories reports whether records belong to more than one repository.
func spansRepositories(records []RegisteredWorktree) bool {
	for _, record := range records[1:] {
		if record.RepositoryID != records[0].RepositoryID {
			return true
		}
	}
	return false
}

// currentRepositoryID is opts.RepositoryID when the caller supplied it, else
// the answer of IdentityService.Repository for this registry's own root ("" for
// outside a repository, or one treerig has not identified).
func (r *Registry) currentRepositoryID(ctx context.Context, opts NameOptions) string {
	if opts.RepositoryID != nil {
		return *opts.RepositoryID
	}
	id, present, _, err := r.Identities.Repository(ctx, r.root)
	if err != nil {
		r.logger.Warn(fmt.Sprintf("Could not tell which repository %s belongs to, so a name shared by several repositories is not narrowed to it: %s",
			text.Printable(r.root), text.OneLine(err.Error(), 200)))
		return ""
	}
	if !present {
		return ""
	}
	return id
}

// resolvePath is the path rule. See Resolve.
//
// The path is matched against each recorded directory by file identity
// (os.SameFile) wherever both exist, walking from the path up through its
// parents, so a case-insensitive filesystem (macOS) that spells a directory two
// ways still finds it, and only a component that no longer exists is compared as
// a string. The first level that any directory matches is the longest match, and
// how many components were walked says whether it is the root itself or
// something inside.
//
// A card that is oversized or does not parse reads as "no card" (ReadIndexCard
// does not fail for either), so that worktree has no directory to match and is
// skipped. Under WithinDirectory that can attribute
// a path inside its directory to an outer worktree; a destructive caller never
// accepts a path that is not a root, so it cannot. A card that exists and
// cannot be read (an I/O failure) fails the lookup, as does a recorded
// directory that cannot be resolved for a reason other than being gone, since
// skipping either could name the wrong worktree.
func (r *Registry) resolvePath(records []RegisteredWorktree, name string, opts NameOptions) (*Resolved, error) {
	target, err := r.pathTarget(name, opts)
	if err != nil {
		return nil, err
	}
	physical, err := canonicalOrGone(target)
	if err != nil {
		return nil, clierr.Wrap(fmt.Sprintf("Could not resolve '%s'", text.Printable(name)), err)
	}

	type recorded struct {
		record RegisteredWorktree
		root   string // the card's own text
		path   string // root, resolved through symbolic links
		info   os.FileInfo
	}
	var roots []recorded
	for _, record := range records {
		host := text.Printable(layout.WorktreeHost(record.Slug, record.Domain))
		seen, err := card.ReadIndexCard(r.ControlHome, record.RepositoryID, record.WorktreeID)
		if err != nil {
			return nil, clierr.Wrap(fmt.Sprintf("Could not read the index card of %s. Name the worktree by its host or its UUID instead", host), err)
		}
		if seen == nil || !usableRoot(seen.Root) {
			continue
		}
		path, err := canonicalOrGone(seen.Root)
		if err != nil {
			return nil, clierr.Wrap(fmt.Sprintf("Could not resolve the directory of %s. Name the worktree by its host or its UUID instead", host), err)
		}
		info, err := os.Stat(path)
		if err != nil && !isGone(err) {
			return nil, clierr.Wrap(fmt.Sprintf("Could not resolve the directory of %s. Name the worktree by its host or its UUID instead", host), err)
		}
		roots = append(roots, recorded{record: record, root: seen.Root, path: path, info: info})
	}

	var best []recorded
	depth := 0
	for dir := physical; ; depth++ {
		info, statErr := os.Stat(dir)
		if statErr != nil && !isGone(statErr) {
			return nil, clierr.Wrap(fmt.Sprintf("Could not resolve '%s'", text.Printable(name)), statErr)
		}
		for _, candidate := range roots {
			same := false
			if info != nil && candidate.info != nil {
				same = os.SameFile(info, candidate.info)
			} else if info == nil && candidate.info == nil {
				// Neither is there any more: all that is left is the text.
				same = dir == candidate.path
			}
			if same {
				best = append(best, candidate)
			}
		}
		if len(best) > 0 {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if len(best) == 0 {
		if opts.WithinDirectory {
			return nil, noWorktree("No worktree is registered with a directory containing %s. "+
				"(A worktree that was never brought up has no directory on record; name it by its host or UUID.)", text.Printable(physical))
		}
		return nil, noWorktree("No worktree is registered with its directory at %s. "+
			"(A worktree that was never brought up has no directory on record; name it by its host or UUID.)", text.Printable(physical))
	}
	if depth > 0 && !opts.WithinDirectory {
		inner := best[0]
		advice := "name the worktree by its host, its UUID or its root directory, " + text.Printable(inner.root) + "."
		if physical == filepath.Clean(opts.Cwd) {
			advice = "Run the command with no name from here, or " + advice
		} else {
			advice = strings.ToUpper(advice[:1]) + advice[1:]
		}
		return nil, clierr.Newf("'%s' is inside the directory of %s (%s), not its root. %s",
			text.Printable(name), text.Printable(layout.WorktreeHost(inner.record.Slug, inner.record.Domain)), inner.record.WorktreeID, advice)
	}
	matched := make([]RegisteredWorktree, len(best))
	for i, candidate := range best {
		matched[i] = candidate.record
	}
	found, err := r.single(name, matched, "")
	if err != nil {
		return nil, err
	}
	found.Root = best[0].root
	return found, nil
}

// usableRoot reports whether a card's recorded directory can take part in
// naming: an absolute path other than "/". A relative one would mean whatever
// the reader's working directory happens to be, and "/" would be the longest
// match for nothing and the parent of everything; both read as no card.
func usableRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) != string(filepath.Separator)
}

// isGone reports whether err says the path is not there: it does not exist, or
// an ancestor of it is not a directory (it became a file), which leaves it just
// as absent.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// canonicalOrGone is CanonicalPath that also lets a path whose ancestor is now
// a regular file through: the deepest part that can be resolved is, and the
// rest is kept as text.
func canonicalOrGone(path string) (string, error) {
	resolved, err := CanonicalPath(path)
	if err == nil || !errors.Is(err, syscall.ENOTDIR) {
		return resolved, err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	head, err := canonicalOrGone(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(head, filepath.Base(path)), nil
}

// pathTarget is name as an absolute, cleaned path: "~" expanded from
// opts.Home (else the registry's own UserHome), a relative form read from
// opts.Cwd. The cleaning is lexical: ".." removes the component before it as
// text, as the shell's own cd -L does, so "link/.." is the directory containing
// link, not link's target's parent. Symbolic links are resolved afterwards, on
// the result.
func (r *Registry) pathTarget(name string, opts NameOptions) (string, error) {
	path := name
	if name == "~" || strings.HasPrefix(name, "~/") {
		home := opts.Home
		if home == "" {
			home = r.userHome
		}
		if home == "" {
			return "", clierr.Newf("Cannot expand '%s': your home directory is not known.", text.Printable(name))
		}
		path = filepath.Join(home, strings.TrimPrefix(name, "~"))
	} else if strings.HasPrefix(name, "~") {
		return "", clierr.Newf("'%s' names another user's home, which treerig does not expand. Write the full path, or '~/<path>' for your own.", text.Printable(name))
	}
	if !filepath.IsAbs(path) {
		if opts.Cwd == "" {
			return "", fmt.Errorf("registry: reading the relative path %q needs NameOptions.Cwd", name)
		}
		path = filepath.Join(opts.Cwd, path)
	}
	return filepath.Clean(path), nil
}

// noWorktree is the failure for a name that matches nothing; errors.Is
// reports ErrNoWorktree for it.
func noWorktree(format string, args ...any) error {
	err := clierr.Newf(format, args...)
	err.Kind = ErrNoWorktree
	return err
}

// NameCandidates is every registered worktree on the machine, each with the
// shortest name Resolve reads back as that worktree alone: its slug when the
// slug is unambiguous for opts, else its host, else its UUID. Shell
// completion offers these, so it never offers a name the command would refuse.
// Sorted by slug, then domain, then UUID. opts.Domain leaves out the worktrees
// of other domains. It reads files only (the records and each index card),
// and asks Git for the current repository only when a slug is shared by
// several repositories.
func (r *Registry) NameCandidates(ctx context.Context, opts NameOptions) ([]NameCandidate, error) {
	scan, err := ScanRegistrations(r.ControlHome)
	if err != nil {
		return nil, err
	}
	bySlug := map[string][]RegisteredWorktree{}
	byHost := map[string]int{}
	for _, record := range scan.Records {
		if opts.Domain != "" && record.Domain != opts.Domain {
			continue
		}
		bySlug[record.Slug] = append(bySlug[record.Slug], record)
	}
	// A host is claimed across every domain, so it is counted unfiltered.
	for _, record := range scan.Records {
		byHost[strings.ToLower(layout.WorktreeHost(record.Slug, record.Domain))]++
	}

	// Cached for the whole listing: every group that needs it asks the same question.
	cached := opts
	asked := false
	narrow := func(group []RegisteredWorktree) []RegisteredWorktree {
		if len(group) > 1 && spansRepositories(group) && !asked {
			id := r.currentRepositoryID(ctx, opts)
			cached.RepositoryID, asked = &id, true
		}
		return r.narrowToRepository(ctx, group, cached)
	}

	var candidates []NameCandidate
	for slug, group := range bySlug {
		narrowed := narrow(group)
		for _, record := range group {
			// The slug only when Resolve would read it as a slug and find this
			// worktree alone; a branch named like a UUID or a path never is.
			name := slug
			if IsUUID(slug) || IsPathName(slug) || len(narrowed) != 1 || narrowed[0].WorktreeID != record.WorktreeID {
				name = layout.WorktreeHost(record.Slug, record.Domain)
				if byHost[strings.ToLower(name)] != 1 || IsPathName(name) {
					name = record.WorktreeID
				}
			}
			root := ""
			if seen, err := card.ReadIndexCard(r.ControlHome, record.RepositoryID, record.WorktreeID); err == nil && seen != nil && usableRoot(seen.Root) {
				root = seen.Root
			}
			candidates = append(candidates, NameCandidate{Name: name, Record: record, Root: root})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].Record, candidates[j].Record
		if a.Slug != b.Slug {
			return a.Slug < b.Slug
		}
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		return a.WorktreeID < b.WorktreeID
	})
	return candidates, nil
}
