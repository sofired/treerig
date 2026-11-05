// Package registry is the worktree registry's read path: the identity
// service (Git-backed UUIDs for a repository and its worktrees), the
// on-disk record store, and the machine-wide scan that lists and finds
// registrations.
//
// Identity is two UUIDs written into the Git directory: one for the
// repository (in the common dir, shared by every worktree) and one for this
// worktree (in its private git dir). Paths and branch names are descriptive
// and may change; the UUIDs are durable. The primary checkout is a worktree
// too, so both cases share one implementation.
//
// The registry is one JSON record per worktree. The domain in a record is a
// cache of a value whose truth is the checkout's committed config file,
// which changes by `git pull` with no central moment to reconcile: writing
// or reconciling a record needs the registry lock, so it is not part of this
// package. Reads never mutate.
package registry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/stateformat"
)

// uuidPattern is zod's `.uuid()` with no version pinned: any RFC 9562/4122
// UUID (version nibble 1-8, variant 8/9/a/b), the nil UUID, or the max UUID.
var uuidPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

var (
	repositoryMarkerRelative = filepath.Join(brand.MarkerDir, "repository-id")
	worktreeMarkerRelative   = filepath.Join(brand.MarkerDir, "worktree-id")
)

// WorktreeIdentity is a checkout's durable identity.
type WorktreeIdentity struct {
	Root         string
	Branch       string
	GitCommonDir string
	GitDir       string
	RepositoryID string
	WorktreeID   string
}

// ListedWorktree is one entry from `git worktree list`.
type ListedWorktree struct {
	Root   string
	Branch string
	// Prunable is Git's own verdict that the entry points at nothing it can
	// use (its directory is gone, or its administrative files are); Git older
	// than 2.31 never says so. PrunableReason is Git's words for it.
	Prunable       bool
	PrunableReason string
	// Locked is an entry `git worktree lock` protects from pruning.
	Locked bool
}

// InspectOptions controls Inspect.
type InspectOptions struct {
	// CreateMarkers creates the identity markers if they are missing, instead
	// of treating a missing worktree marker as "not registered".
	CreateMarkers bool
}

// IdentityService answers identity questions for the checkout at Root, via
// Runner. It never writes a registry record — only the identity markers
// inside .git, which need no lock: an exclusive hard link picks one winner
// among concurrent creators.
type IdentityService struct {
	// Root is the checkout this service answers for.
	Root   string
	runner run.Runner
	// environ is the process environment the first Git question of Repository
	// runs in, with the C locale added so Git's refusal reads the same on every
	// machine. Nil inherits this process's own, and then a localised refusal
	// is an error instead of an answer.
	environ []string
}

// NewIdentityService answers identity questions for the checkout at root.
func NewIdentityService(root string, runner run.Runner) *IdentityService {
	return &IdentityService{Root: root, runner: runner}
}

// gitValue runs git with args in worktree and returns its trimmed stdout.
func (s *IdentityService) gitValue(ctx context.Context, worktree string, args []string) (string, error) {
	full := append([]string{"-C", worktree}, args...)
	result, err := s.runner.Run(ctx, run.Spec{Command: "git", Args: full})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// CanonicalPath is realpath(3) that tolerates a trailing segment which does
// not exist yet: it resolves the longest existing prefix and rejoins
// whatever is missing.
func CanonicalPath(candidate string) (string, error) {
	existing, err := layout.Resolve(candidate)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return layout.Resolve(candidate)
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
}

// assertIdentityMarkerPath refuses a marker whose grandparent directory
// isn't base itself, or whose canonical path differs from marker (a symlink
// or other indirection along the way), so a marker read never follows a
// crafted path outside the checkout it identifies.
func assertIdentityMarkerPath(base, marker string) error {
	canon, err := CanonicalPath(marker)
	if err != nil {
		return err
	}
	if filepath.Dir(filepath.Dir(marker)) != base || canon != marker {
		return clierr.Newf("Unsafe identity marker path: %s", marker)
	}
	return nil
}

// readUUID reads an identity marker, treating a missing file (including a
// dangling symlink) as absent rather than an error.
func readUUID(file string) (value string, present bool, err error) {
	exists, err := files.PathExists(file)
	if err != nil || !exists {
		return "", false, err
	}
	info, err := os.Lstat(file)
	if err != nil {
		return "", false, err
	}
	canon, err := CanonicalPath(file)
	if err != nil {
		return "", false, err
	}
	absFile, err := layout.Resolve(file)
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || canon != absFile {
		return "", false, clierr.Newf("Unsafe identity marker: %s", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false, err
	}
	value, err = parseIdentityMarker(file, string(data))
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// IdentityMarkerContents is what an identity marker holds for id: its format
// line, then the UUID (docs/design/state.md). The marker is two lines so a
// build that does not know its format never mistakes it for a UUID.
func IdentityMarkerContents(id string) string {
	return stateformat.FormatLine(stateformat.IdentityMarker) + "\n" + id + "\n"
}

// parseIdentityMarker reads the UUID out of the marker file's text. A format
// above the one this build writes is a checkout a newer treerig marked, refused
// with the upgrade instruction before anything else is looked at, since its
// layout is not this build's to judge. A marker with no format line, an
// unknown one, or anything else but the UUID after it, is not one treerig wrote:
// it is invalid, like any other garbage in the marker's place.
func parseIdentityMarker(file, text string) (string, error) {
	first, rest, _ := strings.Cut(strings.TrimSuffix(text, "\n"), "\n")
	switch verdict, found := stateformat.Line(first, stateformat.IdentityMarker); verdict {
	case stateformat.FromNewer:
		return "", stateformat.Newer("use", "the identity marker "+file, found, stateformat.IdentityMarker)
	case stateformat.Current:
		if uuidPattern.MatchString(rest) {
			return rest, nil
		}
	}
	return "", clierr.Newf("Invalid identity marker: %s. A marker is the line \"%s\" and then the checkout's UUID. "+
		"To start over, move the file aside: the checkout then gets a new identity, and its old registration is reported by `doctor` for `worktree remove` to clear.",
		file, stateformat.FormatLine(stateformat.IdentityMarker))
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// readOrCreateUUID reads the marker, or mints and stores one. An exclusive
// hard link from a per-caller temp file selects one identity across
// concurrent creators; the loser reads the winner's value back.
func readOrCreateUUID(file string) (string, error) {
	existing, present, err := readUUID(file)
	if err != nil {
		return "", err
	}
	if present {
		return existing, nil
	}

	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	value, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	unique, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	temporary := fmt.Sprintf("%s.tmp.%d.%s", file, os.Getpid(), unique)
	defer os.Remove(temporary)

	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := handle.WriteString(IdentityMarkerContents(value))
	if closeErr := handle.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return "", writeErr
	}

	if err := os.Link(temporary, file); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		winner, present, err := readUUID(file)
		if err != nil {
			return "", err
		}
		if !present {
			return "", clierr.Newf("Identity marker was not created correctly: %s", file)
		}
		return winner, nil
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return "", err
	}
	return value, nil
}

// Inspect answers worktree's identity. A worktree with no worktree marker
// yet (and CreateMarkers false) answers (nil, nil): not registered, not an
// error.
func (s *IdentityService) Inspect(ctx context.Context, worktree string, opts InspectOptions) (*WorktreeIdentity, error) {
	topLevel, err := s.gitValue(ctx, worktree, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return nil, err
	}
	root, err := CanonicalPath(topLevel)
	if err != nil {
		return nil, err
	}
	branch, err := s.gitValue(ctx, root, []string{"branch", "--show-current"})
	if err != nil {
		return nil, err
	}
	rawGitDir, err := s.gitValue(ctx, root, []string{"rev-parse", "--path-format=absolute", "--git-dir"})
	if err != nil {
		return nil, err
	}
	gitDir, err := CanonicalPath(rawGitDir)
	if err != nil {
		return nil, err
	}
	rawCommonDir, err := s.gitValue(ctx, root, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"})
	if err != nil {
		return nil, err
	}
	gitCommonDir, err := CanonicalPath(rawCommonDir)
	if err != nil {
		return nil, err
	}

	repositoryMarker := filepath.Join(gitCommonDir, repositoryMarkerRelative)
	worktreeMarker := filepath.Join(gitDir, worktreeMarkerRelative)
	if err := assertIdentityMarkerPath(gitCommonDir, repositoryMarker); err != nil {
		return nil, err
	}
	if err := assertIdentityMarkerPath(gitDir, worktreeMarker); err != nil {
		return nil, err
	}

	var repositoryID, worktreeID string
	var haveRepositoryID, haveWorktreeID bool
	if opts.CreateMarkers {
		if repositoryID, err = readOrCreateUUID(repositoryMarker); err != nil {
			return nil, err
		}
		haveRepositoryID = true
		if worktreeID, err = readOrCreateUUID(worktreeMarker); err != nil {
			return nil, err
		}
		haveWorktreeID = true
	} else {
		if repositoryID, haveRepositoryID, err = readUUID(repositoryMarker); err != nil {
			return nil, err
		}
		if worktreeID, haveWorktreeID, err = readUUID(worktreeMarker); err != nil {
			return nil, err
		}
	}
	if !haveWorktreeID {
		return nil, nil
	}
	if !haveRepositoryID {
		return nil, clierr.Newf("Incomplete identity markers for %s.", root)
	}
	return &WorktreeIdentity{
		Root: root, Branch: branch, GitCommonDir: gitCommonDir, GitDir: gitDir,
		RepositoryID: repositoryID, WorktreeID: worktreeID,
	}, nil
}

// Ensure is Inspect with marker creation; it never answers nil.
func (s *IdentityService) Ensure(ctx context.Context, worktree string) (*WorktreeIdentity, error) {
	identity, err := s.Inspect(ctx, worktree, InspectOptions{CreateMarkers: true})
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, clierr.Newf("Could not create identity markers for %s.", worktree)
	}
	return identity, nil
}

// notARepository is the text of Git's refusal outside every repository, in
// the C locale Repository asks in.
const notARepository = "not a git repository"

// topLevel asks Git for the top level of the work tree containing worktree.
// Only Git's refusal for a directory outside every repository is returned as
// refusal, with the failure the caller can report as it always has; every
// other failure (an unsafe directory, a corrupt repository, a bare one, Git
// not starting) is err, carrying Git's own words.
func (s *IdentityService) topLevel(ctx context.Context, worktree string) (top string, refusal, err error) {
	spec := run.Spec{
		Command:      "git",
		Args:         []string{"-C", worktree, "rev-parse", "--show-toplevel"},
		AllowFailure: true,
	}
	if s.environ != nil {
		spec.Env = append(slices.Clone(s.environ), "LC_ALL=C")
	}
	result, err := s.runner.Run(ctx, spec)
	if err != nil {
		return "", nil, err
	}
	if result.ExitCode == 0 && !result.TimedOut && !result.OutputOverflow && result.Signal == 0 {
		return strings.TrimSpace(result.Stdout), nil, nil
	}
	failure := run.Failure(spec, result)
	if result.ExitCode != 0 && !result.TimedOut && result.Signal == 0 && strings.Contains(result.Stderr, notARepository) {
		return "", failure, nil
	}
	return "", nil, failure
}

// Repository is the one question every command that may run outside a
// repository asks first: whether worktree is inside a Git work tree at all
// (inside), and if it is, treerig's identity for the repository (id, with
// present false when treerig has none yet). Git's refusal for a directory
// outside every repository is inside false with no error; any other Git
// failure is an error. It runs the same Git calls RepositoryID does.
func (s *IdentityService) Repository(ctx context.Context, worktree string) (id string, present, inside bool, err error) {
	top, refusal, err := s.topLevel(ctx, worktree)
	if err != nil {
		return "", false, false, err
	}
	if refusal != nil {
		return "", false, false, nil
	}
	id, present, err = s.repositoryAt(ctx, top)
	return id, present, true, err
}

// RepositoryID answers just the repository identity, without touching the
// worktree marker. Outside every repository it is an error, as it is for the
// commands that have a treerig.toml and so a repository to be in.
func (s *IdentityService) RepositoryID(ctx context.Context, worktree string) (id string, present bool, err error) {
	top, refusal, err := s.topLevel(ctx, worktree)
	if err != nil {
		return "", false, err
	}
	if refusal != nil {
		return "", false, refusal
	}
	return s.repositoryAt(ctx, top)
}

// repositoryAt is the repository identity of the work tree whose top level Git
// printed.
func (s *IdentityService) repositoryAt(ctx context.Context, topLevel string) (id string, present bool, err error) {
	root, err := CanonicalPath(topLevel)
	if err != nil {
		return "", false, err
	}
	rawCommonDir, err := s.gitValue(ctx, root, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"})
	if err != nil {
		return "", false, err
	}
	gitCommonDir, err := CanonicalPath(rawCommonDir)
	if err != nil {
		return "", false, err
	}
	marker := filepath.Join(gitCommonDir, repositoryMarkerRelative)
	if err := assertIdentityMarkerPath(gitCommonDir, marker); err != nil {
		return "", false, err
	}
	return readUUID(marker)
}

// RemoveWorktreeMarker removes this worktree's identity marker, for
// `worktree remove`. A no-op if it is already gone; refuses if the marker
// now names a different worktree, which would mean identity changed mid-cleanup.
func (s *IdentityService) RemoveWorktreeMarker(identity WorktreeIdentity) error {
	marker := filepath.Join(identity.GitDir, worktreeMarkerRelative)
	if err := assertIdentityMarkerPath(identity.GitDir, marker); err != nil {
		return err
	}
	current, present, err := readUUID(marker)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if current != identity.WorktreeID {
		return clierr.Newf("Worktree identity changed before cleanup completed: %s", identity.Root)
	}
	return os.Remove(marker)
}

// List is `git worktree list --porcelain -z`, parsed.
func (s *IdentityService) List(ctx context.Context) ([]ListedWorktree, error) {
	result, err := s.runner.Run(ctx, run.Spec{
		Command: "git",
		Args:    []string{"-C", s.Root, "worktree", "list", "--porcelain", "-z"},
	})
	if err != nil {
		return nil, err
	}
	var worktrees []ListedWorktree
	for _, block := range strings.Split(result.Stdout, "\x00\x00") {
		var fields []string
		for _, field := range strings.Split(block, "\x00") {
			if field != "" {
				fields = append(fields, field)
			}
		}
		var worktreeDir string
		var found bool
		for _, field := range fields {
			if strings.HasPrefix(field, "worktree ") {
				worktreeDir = field[len("worktree "):]
				found = true
				break
			}
		}
		if !found {
			continue
		}
		branch := ""
		listed := ListedWorktree{}
		for _, field := range fields {
			switch {
			case strings.HasPrefix(field, "branch refs/heads/"):
				if branch == "" {
					branch = field[len("branch refs/heads/"):]
				}
			case field == "prunable" || strings.HasPrefix(field, "prunable "):
				listed.Prunable = true
				listed.PrunableReason = strings.TrimPrefix(strings.TrimPrefix(field, "prunable"), " ")
			case field == "locked" || strings.HasPrefix(field, "locked "):
				listed.Locked = true
			}
		}
		root, err := CanonicalPath(worktreeDir)
		if err != nil {
			return nil, err
		}
		listed.Root, listed.Branch = root, branch
		worktrees = append(worktrees, listed)
	}
	return worktrees, nil
}

// ActiveIdentities is every identity Inspect finds among the worktrees Git
// currently lists and that still exist on disk. A worktree Git lists but
// that has no worktree marker (never registered) is silently skipped.
func (s *IdentityService) ActiveIdentities(ctx context.Context) ([]WorktreeIdentity, error) {
	listed, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var identities []WorktreeIdentity
	for _, worktree := range listed {
		exists, err := files.PathExists(worktree.Root)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		identity, err := s.Inspect(ctx, worktree.Root, InspectOptions{})
		if err != nil {
			return nil, err
		}
		if identity != nil {
			identities = append(identities, *identity)
		}
	}
	return identities, nil
}

// WorktreeEntry is one worktree Git lists, with what is on disk beneath it and
// the treerig identity it carries, for deciding what became of a registration.
type WorktreeEntry struct {
	ListedWorktree
	// Exists is whether Root is there; StatErr is why that could not be told
	// (a permission error, say), in which case Exists is false.
	Exists  bool
	StatErr error
	// RepositoryID and WorktreeID are the entry's identity, empty when it has
	// none yet (never registered) or it could not be read (IdentityErr).
	RepositoryID string
	WorktreeID   string
	IdentityErr  error
}

// Entries is every worktree Git lists, each with its identity, and without
// the failure of one stopping the rest: a marker one entry carries that this
// build cannot read is that entry's IdentityErr, where ActiveIdentities would
// fail the whole call. A directory that is gone (a prunable entry) keeps its
// identity in its Git directory, so it is read from there; Git itself failing
// is the call's error.
func (s *IdentityService) Entries(ctx context.Context) ([]WorktreeEntry, error) {
	listed, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]WorktreeEntry, len(listed))
	var commonDir, repositoryID string
	var commonErr error
	var haveCommon bool
	common := func() (string, string, error) {
		if haveCommon {
			return commonDir, repositoryID, commonErr
		}
		haveCommon = true
		raw, err := s.gitValue(ctx, s.Root, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"})
		if err == nil {
			commonDir, err = CanonicalPath(raw)
		}
		if err == nil {
			marker := filepath.Join(commonDir, repositoryMarkerRelative)
			if err = assertIdentityMarkerPath(commonDir, marker); err == nil {
				repositoryID, _, err = readUUID(marker)
			}
		}
		commonErr = err
		return commonDir, repositoryID, commonErr
	}
	for i, worktree := range listed {
		entry := WorktreeEntry{ListedWorktree: worktree}
		info, statErr := os.Stat(worktree.Root)
		switch {
		case statErr == nil:
			entry.Exists = info.IsDir()
		case !errors.Is(statErr, fs.ErrNotExist):
			entry.StatErr = statErr
		}
		switch {
		case entry.StatErr != nil:
		case entry.Exists && !worktree.Prunable:
			identity, err := s.Inspect(ctx, worktree.Root, InspectOptions{})
			switch {
			case err != nil:
				entry.IdentityErr = err
			case identity != nil:
				entry.RepositoryID, entry.WorktreeID = identity.RepositoryID, identity.WorktreeID
			}
		case i > 0:
			// Gone, or damaged: Git's directory for the entry still holds the
			// marker, but only while Git has not pruned it.
			dir, repository, err := common()
			if err != nil {
				entry.IdentityErr = err
				break
			}
			id, err := identityFromGitDir(dir, worktree.Root)
			entry.WorktreeID, entry.IdentityErr = id, err
			if id != "" {
				entry.RepositoryID = repository
			}
		}
		entries[i] = entry
	}
	return entries, nil
}

// identityFromGitDir finds the worktree whose files were at root among the
// linked worktrees Git keeps under commonDir/worktrees, and reads its
// worktree marker. It answers "" with no error for one that is not there or
// carries no marker.
func identityFromGitDir(commonDir, root string) (string, error) {
	administrative := filepath.Join(commonDir, "worktrees")
	children, err := os.ReadDir(administrative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	want := filepath.Join(root, ".git")
	for _, child := range children {
		if !child.IsDir() {
			continue
		}
		dir := filepath.Join(administrative, child.Name())
		data, err := os.ReadFile(filepath.Join(dir, "gitdir"))
		if err != nil {
			continue
		}
		recorded := strings.TrimSpace(string(data))
		if !filepath.IsAbs(recorded) {
			recorded = filepath.Join(dir, recorded)
		}
		canonical, err := CanonicalPath(recorded)
		if err != nil || canonical != want {
			continue
		}
		canonicalDir, err := CanonicalPath(dir)
		if err != nil {
			return "", err
		}
		marker := filepath.Join(canonicalDir, worktreeMarkerRelative)
		if err := assertIdentityMarkerPath(canonicalDir, marker); err != nil {
			return "", err
		}
		id, _, err := readUUID(marker)
		return id, err
	}
	return "", nil
}
