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
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
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
}

// NewIdentityService answers identity questions for the checkout at root.
func NewIdentityService(root string, runner run.Runner) *IdentityService {
	return &IdentityService{Root: root, runner: runner}
}

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
	value = strings.TrimSpace(string(data))
	if !uuidPattern.MatchString(value) {
		return "", false, clierr.Newf("Invalid identity marker: %s", file)
	}
	return value, true, nil
}

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
	_, writeErr := handle.WriteString(value + "\n")
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

// RepositoryID answers just the repository identity, without touching the
// worktree marker.
func (s *IdentityService) RepositoryID(ctx context.Context, worktree string) (id string, present bool, err error) {
	topLevel, err := s.gitValue(ctx, worktree, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return "", false, err
	}
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
		for _, field := range fields {
			if strings.HasPrefix(field, "branch refs/heads/") {
				branch = field[len("branch refs/heads/"):]
				break
			}
		}
		root, err := CanonicalPath(worktreeDir)
		if err != nil {
			return nil, err
		}
		worktrees = append(worktrees, ListedWorktree{Root: root, Branch: branch})
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
