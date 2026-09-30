package registry

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// Store is the read side of one repository's registrations: one JSON record
// per worktree.
//
// The write side — MarkPending and Remove in this build, in write.go — takes
// the registry lock for every write: writing a record while another process
// reads or writes the same one is exactly the race the lock exists to
// prevent. The read side here is safe without it, because
// a reader never has to see a consistent view across two records — an
// individual read either lands on a complete write (atomicJson's rename is
// atomic) or on the previous complete write.
type Store struct {
	ControlHome  string
	RepositoryID string
	Directory    string
}

// NewStore is the registrations for one repository, sharing controlHome.
func NewStore(controlHome, repositoryID string) *Store {
	return &Store{
		ControlHome:  controlHome,
		RepositoryID: repositoryID,
		Directory:    RegistryDirectory(controlHome, repositoryID),
	}
}

// RecordPath is where worktreeID's record lives.
func (s *Store) RecordPath(worktreeID string) string {
	return layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID).RecordPath
}

// readStoredRecord reads one record, converting every way it can be wrong
// into a *clierr.Error so a stray file never surfaces as a raw parser
// error. A missing file is left alone (a *fs.PathError wrapping
// fs.ErrNotExist): a file that vanished mid-scan is not a problem, and
// callers decide.
func (s *Store) readStoredRecord(file string) (*storedRecord, error) {
	base := strings.TrimSuffix(filepath.Base(file), ".json")
	if !uuidPattern.MatchString(base) {
		return nil, clierr.Newf("Registry file is not named by a worktree UUID: %s", file)
	}

	// The tool only ever writes regular files here. A symlink is either
	// tampering or a sync client, and either way it is a record we cannot
	// vouch for — so it is reported, never followed and never ignored.
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, clierr.Newf("Registry record must be a regular file, not a symbolic link: %s", file)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	parsed, err := parseRecordJSON(data)
	if err != nil {
		if errors.Is(err, errInvalidJSON) {
			return nil, clierr.Newf("Registry record is not valid JSON: %s", file)
		}
		return nil, clierr.Newf("Registry record has an unrecognized version or shape: %s", file)
	}
	if parsed.RepositoryID != s.RepositoryID || parsed.WorktreeID != base {
		return nil, clierr.Newf("Worktree registration identity does not match its path: %s", file)
	}
	return &storedRecord{Initialization: parsed.Initialization, Registration: parsed.RegisteredWorktree}, nil
}

// Scan reads every readable record in this repository, plus the files that
// were not.
func (s *Store) Scan() ([]RegisteredWorktree, []RegistryProblem, error) {
	entries, err := os.ReadDir(s.Directory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var records []RegisteredWorktree
	var problems []RegistryProblem
	for _, entry := range entries {
		// Directories are skipped; everything else named like a record —
		// including a symlink — goes through readStoredRecord so it is
		// reported if it is wrong.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(s.Directory, entry.Name())
		stored, err := s.readStoredRecord(file)
		if err != nil {
			// Removed between readdir and readFile: another process's remove() won.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			problems = append(problems, RegistryProblem{File: file, Reason: err.Error()})
			continue
		}
		records = append(records, stored.Registration)
	}
	return records, problems, nil
}

// List is every readable record, sorted by slug. Slugs are drawn from
// [a-z0-9-], so Go's byte-order string comparison is
// `localeCompare(…, 'en')` for this alphabet: see TestListSortMatchesLocale.
func (s *Store) List() ([]RegisteredWorktree, error) {
	records, _, err := s.Scan()
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Slug < records[j].Slug })
	return records, nil
}

// Find reads one record, or answers (nil, nil) when it does not exist.
func (s *Store) Find(worktreeID string) (*RegisteredWorktree, error) {
	stored, err := s.readStoredRecord(s.RecordPath(worktreeID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &stored.Registration, nil
}

// Initialization reads a record's initialization state, refusing a record
// that no longer belongs to expectedSlug.
func (s *Store) Initialization(worktreeID, expectedSlug string) (WorktreeInitialization, error) {
	file := s.RecordPath(worktreeID)
	exists, err := files.PathExists(file)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", clierr.Newf("Worktree registration is missing: %s", file)
	}
	stored, err := s.readStoredRecord(file)
	if err != nil {
		return "", err
	}
	if stored.Registration.Slug != expectedSlug {
		return "", clierr.Newf("Worktree %s no longer owns registration %s", expectedSlug, file)
	}
	return stored.Initialization, nil
}
